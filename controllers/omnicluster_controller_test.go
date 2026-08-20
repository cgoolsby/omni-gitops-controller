package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/cosi-project/runtime/pkg/state/impl/inmem"
	"github.com/cosi-project/runtime/pkg/state/impl/namespaced"
	omniSpecs "github.com/siderolabs/omni/client/api/omni/specs"
	omniresources "github.com/siderolabs/omni/client/pkg/omni/resources"
	omnires "github.com/siderolabs/omni/client/pkg/omni/resources/omni"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/cgoolsby/omni-gitops-controller/api/v1alpha1"
)

// newTestState creates a real in-memory COSI state suitable for unit tests.
func newTestState() state.State {
	return state.WrapCore(namespaced.NewState(inmem.Build))
}

// newTestOmniClient creates an OmniClient backed by an in-memory COSI state.
func newTestOmniClient(st state.State) *OmniClient {
	return &OmniClient{state: st}
}

// newTestScheme returns a runtime.Scheme with corev1 and the omni.gitops.dev
// API types registered, for use with the controller-runtime fake client.
func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatalf("add api to scheme: %v", err)
	}
	return scheme
}

// mustJSON marshals v to compact JSON or panics.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// ── EnsureConfigPatch tests ────────────────────────────────────────────────────

// TestEnsureConfigPatch_UpdateOnChange is a regression test for the
// "create-once, never-update" bug: changing the inline content of an
// OmniCluster ConfigPatch must propagate to the Omni resource.
func TestEnsureConfigPatch_UpdateOnChange(t *testing.T) {
	ctx := context.Background()
	st := newTestState()
	c := newTestOmniClient(st)

	clusterName := "test-cluster"
	machineSetID := "test-cluster-workers"
	machineID := "machine-uuid-1"
	patchID := fmt.Sprintf("%s-%s-%s", clusterName, machineID, "install-disk")

	oldData := json.RawMessage(mustJSON(map[string]any{
		"machine": map[string]any{
			"install": map[string]any{
				"disk": "/dev/sda",
				"wipe": true,
			},
		},
	}))

	// First call: patch doesn't exist → Create.
	if err := c.EnsureConfigPatch(ctx, patchID, clusterName, machineSetID, machineID, oldData); err != nil {
		t.Fatalf("first EnsureConfigPatch: %v", err)
	}

	newData := json.RawMessage(mustJSON(map[string]any{
		"machine": map[string]any{
			"install": map[string]any{
				"disk": "/dev/sdb",
				"wipe": false,
			},
		},
	}))

	// Second call: content changed → must Update.
	if err := c.EnsureConfigPatch(ctx, patchID, clusterName, machineSetID, machineID, newData); err != nil {
		t.Fatalf("second EnsureConfigPatch: %v", err)
	}

	// Read back and verify the updated content.
	md := resource.NewMetadata(omniresources.DefaultNamespace, omnires.ConfigPatchType, patchID, resource.VersionUndefined)
	stored, err := safe.StateGet[*omnires.ConfigPatch](ctx, st, md)
	if err != nil {
		t.Fatalf("read back config patch: %v", err)
	}

	buf, err := stored.TypedSpec().Value.GetUncompressedData()
	if err != nil {
		t.Fatalf("decompress stored data: %v", err)
	}
	defer buf.Free()

	if !jsonEqual(buf.Data(), newData) {
		t.Errorf("stored config patch content not updated:\n  got  %s\n  want %s", buf.Data(), newData)
	}
}

// TestEnsureConfigPatch_NoUpdateWhenUnchanged verifies that a ConfigPatch
// that is semantically identical (but different bytes, e.g. different key
// ordering or whitespace) does NOT trigger a spurious Update call.
func TestEnsureConfigPatch_NoUpdateWhenUnchanged(t *testing.T) {
	ctx := context.Background()
	st := newTestState()
	c := newTestOmniClient(st)

	clusterName := "test-cluster"
	machineSetID := "test-cluster-workers"
	machineID := "machine-uuid-2"
	patchID := fmt.Sprintf("%s-%s-%s", clusterName, machineID, "install-disk")

	// Create with compact JSON.
	compact := json.RawMessage(`{"machine":{"install":{"disk":"/dev/sda"}}}`)
	if err := c.EnsureConfigPatch(ctx, patchID, clusterName, machineSetID, machineID, compact); err != nil {
		t.Fatalf("create patch: %v", err)
	}

	// Record the resource version after creation.
	md := resource.NewMetadata(omniresources.DefaultNamespace, omnires.ConfigPatchType, patchID, resource.VersionUndefined)
	before, err := safe.StateGet[*omnires.ConfigPatch](ctx, st, md)
	if err != nil {
		t.Fatalf("read resource version: %v", err)
	}
	versionBefore := before.Metadata().Version()

	// Call again with semantically identical JSON (different whitespace / key order).
	spaced := json.RawMessage(`{"machine": {"install": {"disk": "/dev/sda"}}}`)
	if err := c.EnsureConfigPatch(ctx, patchID, clusterName, machineSetID, machineID, spaced); err != nil {
		t.Fatalf("no-op EnsureConfigPatch: %v", err)
	}

	after, err := safe.StateGet[*omnires.ConfigPatch](ctx, st, md)
	if err != nil {
		t.Fatalf("read resource version after no-op: %v", err)
	}

	if !after.Metadata().Version().Equal(versionBefore) {
		t.Errorf("resource version bumped on semantically-identical JSON: %v → %v",
			versionBefore, after.Metadata().Version())
	}
}

// ── setup helpers ─────────────────────────────────────────────────────────────

// setupAllocatedMachine pre-creates a MachineSetNode in the in-memory state so
// that AllocatedMachineSetNodes returns it without needing a real Omni server.
func setupAllocatedMachine(t *testing.T, ctx context.Context, c *OmniClient, clusterName, machineSetID, machineID, role string) {
	t.Helper()
	if err := c.EnsureMachineSetNode(ctx, clusterName, machineSetID, machineID, role); err != nil {
		t.Fatalf("pre-create MachineSetNode: %v", err)
	}
}

// ── reconcileMachineSetAllocation tests ───────────────────────────────────────

// newAllocReconciler builds an OmniClusterReconciler wired with a fake
// controller-runtime client (holding the given Machine objects) and an
// in-memory OmniClient, for exercising the Machine-object allocation paths.
func newAllocReconciler(t *testing.T, c *OmniClient, machines ...*api.Machine) *OmniClusterReconciler {
	t.Helper()
	scheme := newTestScheme(t)
	objs := make([]client.Object, 0, len(machines))
	for _, m := range machines {
		objs = append(objs, m)
	}
	return &OmniClusterReconciler{
		Client:     fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
		Scheme:     scheme,
		OmniClient: c,
	}
}

// newAllocatedMachine builds a Machine object as the cluster controller would
// create it for an already-allocated machine.
func newAllocatedMachine(clusterName, machineSetID, role, machineID string) *api.Machine {
	return &api.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      machineID,
			Namespace: "default",
			Labels: map[string]string{
				labelCluster:    clusterName,
				labelMachineSet: machineSetID,
			},
		},
		Spec: api.MachineSpec{
			ClusterName:  clusterName,
			MachineSetID: machineSetID,
			Role:         role,
		},
	}
}

// TestReconcileMachineSetAllocation_ScaleUpCreatesMachines verifies that a set
// short of its desired replicas selects available Omni machines and creates
// Machine objects for them, owned by the cluster.
func TestReconcileMachineSetAllocation_ScaleUpCreatesMachines(t *testing.T) {
	ctx := context.Background()
	st := newTestState()
	c := newTestOmniClient(st)

	createMachineStatus(ctx, t, st, "worker-uuid-1", true, nil)
	createMachineStatus(ctx, t, st, "worker-uuid-2", true, nil)

	clusterName := "test-cluster"
	machineSetID := "test-cluster-workers"
	r := newAllocReconciler(t, c)
	cluster := &api.OmniCluster{ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: "default"}}

	allocated, err := r.reconcileMachineSetAllocation(ctx, cluster, clusterName, machineSetID, roleWorker,
		api.MachineSetSpec{Replicas: 2}, map[string]bool{})
	if err != nil {
		t.Fatalf("reconcileMachineSetAllocation: %v", err)
	}
	if len(allocated) != 2 {
		t.Fatalf("expected 2 allocated machines, got %v", allocated)
	}

	list := &api.MachineList{}
	if err := r.List(ctx, list); err != nil {
		t.Fatalf("list machines: %v", err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("expected 2 Machine objects created, got %d", len(list.Items))
	}
	for _, m := range list.Items {
		if m.Spec.ClusterName != clusterName || m.Spec.MachineSetID != machineSetID || m.Spec.Role != roleWorker {
			t.Errorf("machine %s has unexpected spec: %+v", m.Name, m.Spec)
		}
		if len(m.OwnerReferences) != 1 || m.OwnerReferences[0].Name != clusterName {
			t.Errorf("machine %s missing controller owner reference: %+v", m.Name, m.OwnerReferences)
		}
		if m.Labels[labelCluster] != clusterName || m.Labels[labelMachineSet] != machineSetID {
			t.Errorf("machine %s missing allocation labels: %+v", m.Name, m.Labels)
		}
	}
}

// TestReconcileMachineSetAllocation_ControlPlaneScaleDownOneAtATime verifies that
// control-plane scale-down deletes at most one Machine per reconcile and never
// starts a new removal while one is still tearing down.
func TestReconcileMachineSetAllocation_ControlPlaneScaleDownOneAtATime(t *testing.T) {
	ctx := context.Background()
	c := newTestOmniClient(newTestState())

	clusterName := "test-cluster"
	machineSetID := omnires.ControlPlanesResourceID(clusterName)

	var machines []*api.Machine
	for i := 1; i <= 3; i++ {
		machines = append(machines, newAllocatedMachine(clusterName, machineSetID, roleControlPlane, fmt.Sprintf("cp-uuid-%d", i)))
	}
	r := newAllocReconciler(t, c, machines...)
	cluster := &api.OmniCluster{ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: "default"}}
	spec := api.MachineSetSpec{Replicas: 1}

	allocated, err := r.reconcileMachineSetAllocation(ctx, cluster, clusterName, machineSetID, roleControlPlane, spec, map[string]bool{})
	if err != nil {
		t.Fatalf("first scale-down: %v", err)
	}
	if len(allocated) != 2 {
		t.Fatalf("first scale-down: kept %d, want 2 (one removed per cycle)", len(allocated))
	}
	// The fake client has no finalizers, so the deleted Machine is actually gone.
	remaining := &api.MachineList{}
	if err := r.List(ctx, remaining); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(remaining.Items) != 2 {
		t.Fatalf("first scale-down: %d Machine objects remain, want 2", len(remaining.Items))
	}

	allocated, err = r.reconcileMachineSetAllocation(ctx, cluster, clusterName, machineSetID, roleControlPlane, spec, map[string]bool{})
	if err != nil {
		t.Fatalf("second scale-down: %v", err)
	}
	if len(allocated) != 1 {
		t.Errorf("second scale-down: kept %d, want 1", len(allocated))
	}
}

// TestReconcileMachineSetAllocation_WorkerScaleDownBatch verifies worker
// scale-down deletes all excess Machine objects in a single reconcile.
func TestReconcileMachineSetAllocation_WorkerScaleDownBatch(t *testing.T) {
	ctx := context.Background()
	c := newTestOmniClient(newTestState())

	clusterName := "test-cluster"
	machineSetID := "test-cluster-workers"

	var machines []*api.Machine
	for i := 1; i <= 3; i++ {
		machines = append(machines, newAllocatedMachine(clusterName, machineSetID, roleWorker, fmt.Sprintf("worker-uuid-%d", i)))
	}
	r := newAllocReconciler(t, c, machines...)
	cluster := &api.OmniCluster{ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: "default"}}

	allocated, err := r.reconcileMachineSetAllocation(ctx, cluster, clusterName, machineSetID, roleWorker,
		api.MachineSetSpec{Replicas: 1}, map[string]bool{})
	if err != nil {
		t.Fatalf("scale-down: %v", err)
	}
	if len(allocated) != 1 {
		t.Errorf("scale-down: kept %d, want 1 (workers scale down in one batch)", len(allocated))
	}
	remaining := &api.MachineList{}
	if err := r.List(ctx, remaining); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(remaining.Items) != 1 {
		t.Errorf("scale-down: %d Machine objects remain, want 1", len(remaining.Items))
	}
}

// TestReconcileMachineSetAllocation_SyncsSpec verifies that changing the machine
// set spec propagates to already-allocated Machine objects, while preserving the
// reboot-signaling fields owned by the drift path.
func TestReconcileMachineSetAllocation_SyncsSpec(t *testing.T) {
	ctx := context.Background()
	c := newTestOmniClient(newTestState())

	clusterName := "test-cluster"
	machineSetID := "test-cluster-workers"
	m := newAllocatedMachine(clusterName, machineSetID, roleWorker, "worker-uuid-1")
	rebootAt := metav1.NewTime(time.Now().Truncate(time.Second))
	m.Spec.RebootRequestedAt = &rebootAt
	m.Spec.ManagementAddress = "10.0.0.9"
	r := newAllocReconciler(t, c, m)
	cluster := &api.OmniCluster{ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: "default"}}

	spec := api.MachineSetSpec{
		Replicas:   1,
		KernelArgs: []string{"libata.force=noncq"},
		ConfigPatches: []api.ConfigPatch{
			{Name: "install-disk", Inline: apiextensionsv1.JSON{Raw: mustJSON(map[string]any{"machine": map[string]any{"install": map[string]any{"disk": "/dev/sda"}}})}},
		},
	}
	if _, err := r.reconcileMachineSetAllocation(ctx, cluster, clusterName, machineSetID, roleWorker, spec, map[string]bool{}); err != nil {
		t.Fatalf("reconcileMachineSetAllocation: %v", err)
	}

	got := &api.Machine{}
	if err := r.Get(ctx, client.ObjectKey{Name: "worker-uuid-1", Namespace: "default"}, got); err != nil {
		t.Fatalf("get machine: %v", err)
	}
	if len(got.Spec.KernelArgs) != 1 || got.Spec.KernelArgs[0] != "libata.force=noncq" {
		t.Errorf("kernel args not synced: %v", got.Spec.KernelArgs)
	}
	if len(got.Spec.ConfigPatches) != 1 || got.Spec.ConfigPatches[0].Name != "install-disk" {
		t.Errorf("config patches not synced: %v", got.Spec.ConfigPatches)
	}
	if got.Spec.RebootRequestedAt == nil || !got.Spec.RebootRequestedAt.Equal(&rebootAt) {
		t.Errorf("reboot-signaling fields not preserved: %v", got.Spec.RebootRequestedAt)
	}
	if got.Spec.ManagementAddress != "10.0.0.9" {
		t.Errorf("management address not preserved: %q", got.Spec.ManagementAddress)
	}
}

// TestReconcileMachineSetAllocation_PrunesExtensionsWhenEmpty verifies that
// clearing spec.MachineExtensions deletes the machine-set ExtensionsConfiguration
// (extensions stay cluster-controller-owned in the new split).
func TestReconcileMachineSetAllocation_PrunesExtensionsWhenEmpty(t *testing.T) {
	ctx := context.Background()
	st := newTestState()
	c := newTestOmniClient(st)

	clusterName := "test-cluster"
	machineSetID := "test-cluster-workers"
	m := newAllocatedMachine(clusterName, machineSetID, roleWorker, "worker-uuid-1")
	r := newAllocReconciler(t, c, m)
	cluster := &api.OmniCluster{ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: "default"}}

	specWithExts := api.MachineSetSpec{Replicas: 1, MachineExtensions: []string{"siderolabs/iscsi-tools"}}
	if _, err := r.reconcileMachineSetAllocation(ctx, cluster, clusterName, machineSetID, roleWorker, specWithExts, map[string]bool{}); err != nil {
		t.Fatalf("reconcile with extensions: %v", err)
	}
	extID := "schematic-" + machineSetID
	md := resource.NewMetadata(omniresources.DefaultNamespace, omnires.ExtensionsConfigurationType, extID, resource.VersionUndefined)
	if _, err := safe.StateGet[*omnires.ExtensionsConfiguration](ctx, st, md); err != nil {
		t.Fatalf("ExtensionsConfiguration should exist: %v", err)
	}

	specEmpty := api.MachineSetSpec{Replicas: 1}
	if _, err := r.reconcileMachineSetAllocation(ctx, cluster, clusterName, machineSetID, roleWorker, specEmpty, map[string]bool{}); err != nil {
		t.Fatalf("reconcile with empty extensions: %v", err)
	}
	if _, err := safe.StateGet[*omnires.ExtensionsConfiguration](ctx, st, md); err == nil {
		t.Error("ExtensionsConfiguration should have been deleted when spec.MachineExtensions is empty")
	} else if !state.IsNotFoundError(err) {
		t.Fatalf("unexpected error checking deleted ExtensionsConfiguration: %v", err)
	}
}

// TestReconcileMachineSetAllocation_EmitsMachinesAllocatedEvent verifies that
// creating fresh Machine objects emits a MachinesAllocated event.
func TestReconcileMachineSetAllocation_EmitsMachinesAllocatedEvent(t *testing.T) {
	ctx := context.Background()
	st := newTestState()
	c := newTestOmniClient(st)

	createMachineStatus(ctx, t, st, "machine-uuid-9", true, nil)

	clusterName := "test-cluster"
	r := newAllocReconciler(t, c)
	r.Recorder = events.NewFakeRecorder(10)
	cluster := &api.OmniCluster{ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: "default"}}

	allocated, err := r.reconcileMachineSetAllocation(ctx, cluster, clusterName, "test-cluster-workers",
		roleWorker, api.MachineSetSpec{Replicas: 1}, map[string]bool{})
	if err != nil {
		t.Fatalf("reconcileMachineSetAllocation: %v", err)
	}
	if len(allocated) != 1 {
		t.Fatalf("expected 1 allocated machine, got %v", allocated)
	}

	recorder := r.Recorder.(*events.FakeRecorder)
	select {
	case ev := <-recorder.Events:
		if !strings.Contains(ev, "MachinesAllocated") {
			t.Errorf("expected MachinesAllocated event, got %q", ev)
		}
	default:
		t.Error("expected a MachinesAllocated event, got none")
	}
}

// ── deleteOmniResources tests ──────────────────────────────────────────────────

// TestDeleteOmniResources_DeletesKubeconfigSecrets verifies that deleting an
// OmniCluster removes both the Flux-format and ArgoCD-format kubeconfig
// Secrets, and that a missing Secret is tolerated.
func TestDeleteOmniResources_DeletesKubeconfigSecrets(t *testing.T) {
	ctx := context.Background()
	st := newTestState()
	c := newTestOmniClient(st)

	scheme := newTestScheme(t)

	namespace := "flux-system"
	clusterName := "test-cluster"

	fluxSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName + "-kubeconfig", Namespace: namespace},
	}
	argoSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName + "-cluster-secret", Namespace: namespace},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(fluxSecret, argoSecret).Build()

	r := &OmniClusterReconciler{
		Client:              kubeClient,
		OmniClient:          c,
		KubeconfigNamespace: namespace,
	}

	cluster := &api.OmniCluster{ObjectMeta: metav1.ObjectMeta{Name: clusterName}}
	if err := r.deleteOmniResources(ctx, cluster); err != nil {
		t.Fatalf("deleteOmniResources: %v", err)
	}

	for _, name := range []string{clusterName + "-kubeconfig", clusterName + "-cluster-secret"} {
		err := kubeClient.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &corev1.Secret{})
		if err == nil {
			t.Errorf("secret %s should have been deleted", name)
		} else if !apierrors.IsNotFound(err) {
			t.Fatalf("unexpected error getting secret %s: %v", name, err)
		}
	}

	// A second run with no Secrets present must tolerate NotFound.
	if err := r.deleteOmniResources(ctx, cluster); err != nil {
		t.Fatalf("deleteOmniResources with no secrets: %v", err)
	}
}

// TestDeleteOmniResources_DeletesMachinesAndWaits verifies that deleting an
// OmniCluster deletes the Machine objects it owns and blocks (returns a
// retryable error) while any are still finishing teardown via their finalizer,
// before touching the Omni-side machine set / cluster resources.
func TestDeleteOmniResources_DeletesMachinesAndWaits(t *testing.T) {
	ctx := context.Background()
	c := newTestOmniClient(newTestState())

	clusterName := "test-cluster"
	namespace := "flux-system"
	machine := &api.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "machine-uuid-1",
			Namespace:  namespace,
			Labels:     map[string]string{labelCluster: clusterName, labelMachineSet: clusterName + "-workers"},
			Finalizers: []string{machineFinalizerName},
		},
		Spec: api.MachineSpec{ClusterName: clusterName, MachineSetID: clusterName + "-workers", Role: roleWorker},
	}
	scheme := newTestScheme(t)
	r := &OmniClusterReconciler{
		Client:              fake.NewClientBuilder().WithScheme(scheme).WithObjects(machine).WithStatusSubresource(&api.Machine{}).Build(),
		Scheme:              scheme,
		OmniClient:          c,
		KubeconfigNamespace: namespace,
	}
	cluster := &api.OmniCluster{ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace}}

	// The Machine has a finalizer, so it lingers after deletion → wait.
	err := r.deleteOmniResources(ctx, cluster)
	if err == nil {
		t.Fatal("expected deleteOmniResources to wait while a machine is still tearing down")
	}
	if !strings.Contains(err.Error(), "waiting for") {
		t.Errorf("expected a waiting error, got: %v", err)
	}

	// The Machine object was marked for deletion.
	got := &api.Machine{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(machine), got); err != nil {
		t.Fatalf("get machine: %v", err)
	}
	if got.DeletionTimestamp.IsZero() {
		t.Error("machine should have been marked for deletion")
	}

	// Simulate the MachineReconciler finishing teardown (finalizer cleared).
	got.Finalizers = nil
	if err := r.Update(ctx, got); err != nil {
		t.Fatalf("clear finalizer: %v", err)
	}

	// Now teardown proceeds to completion.
	if err := r.deleteOmniResources(ctx, cluster); err != nil {
		t.Fatalf("deleteOmniResources after machines gone: %v", err)
	}
}

// TestDeleteOmniResources_DrivenByOmniState verifies that finalizer teardown
// discovers resources from Omni state (the authoritative source) rather than
// status.AllocatedMachines, so a CR deleted with an empty or stale status map —
// e.g. before its first successful reconcile — does not leak MachineSetNodes,
// MachineSets, ConfigPatches, KernelArgs, or ExtensionsConfigurations in Omni.
func TestDeleteOmniResources_DrivenByOmniState(t *testing.T) {
	ctx := context.Background()
	st := newTestState()
	c := newTestOmniClient(st)

	clusterName := "test-cluster"
	cpSetID := clusterName + "-control-planes"
	workerSetID := clusterName + "-workers"
	cpMachineID := "cp-uuid-1"
	workerMachineID := "worker-uuid-1"

	if err := c.EnsureCluster(ctx, clusterName, "flux-system/"+clusterName, "v1.30.0", "v1.8.0"); err != nil {
		t.Fatalf("ensure cluster: %v", err)
	}
	sets := []struct {
		setID, machineID, role string
	}{
		{cpSetID, cpMachineID, omnires.LabelControlPlaneRole},
		{workerSetID, workerMachineID, omnires.LabelWorkerRole},
	}
	for _, s := range sets {
		if err := c.EnsureMachineSet(ctx, clusterName, s.setID, s.role); err != nil {
			t.Fatalf("ensure machine set %s: %v", s.setID, err)
		}
		if err := c.EnsureMachineSetNode(ctx, clusterName, s.setID, s.machineID, s.role); err != nil {
			t.Fatalf("ensure machine set node %s: %v", s.machineID, err)
		}
		patchID := fmt.Sprintf("%s-%s-%s", clusterName, s.machineID, "install-disk")
		inline := json.RawMessage(mustJSON(map[string]any{"machine": map[string]any{"install": map[string]any{"disk": "/dev/sda"}}}))
		if err := c.EnsureConfigPatch(ctx, patchID, clusterName, s.setID, s.machineID, inline); err != nil {
			t.Fatalf("ensure config patch %s: %v", patchID, err)
		}
		if err := c.EnsureMachineKernelArgs(ctx, s.machineID, []string{"libata.force=noncq"}); err != nil {
			t.Fatalf("ensure kernel args %s: %v", s.machineID, err)
		}
		if err := c.EnsureMachineSetExtensionsConfiguration(ctx, clusterName, s.setID, []string{"siderolabs/iscsi-tools"}); err != nil {
			t.Fatalf("ensure extensions configuration %s: %v", s.setID, err)
		}
	}

	scheme := newTestScheme(t)
	r := &OmniClusterReconciler{
		Client:              fake.NewClientBuilder().WithScheme(scheme).Build(),
		OmniClient:          c,
		KubeconfigNamespace: "flux-system",
	}

	// Status.AllocatedMachines is intentionally empty: this is the failure
	// mode being fixed (CR deleted before status was ever populated).
	cluster := &api.OmniCluster{ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: "flux-system"}}

	if err := r.deleteOmniResources(ctx, cluster); err != nil {
		t.Fatalf("deleteOmniResources: %v", err)
	}

	assertGone := func(md resource.Metadata, desc string) {
		t.Helper()
		_, err := st.Get(ctx, md)
		if err == nil {
			t.Errorf("%s should have been deleted but still exists", desc)
		} else if !state.IsNotFoundError(err) {
			t.Fatalf("unexpected error checking %s: %v", desc, err)
		}
	}

	for _, s := range sets {
		assertGone(resource.NewMetadata(omniresources.DefaultNamespace, omnires.MachineSetNodeType, s.machineID, resource.VersionUndefined), "MachineSetNode "+s.machineID)
		patchID := fmt.Sprintf("%s-%s-%s", clusterName, s.machineID, "install-disk")
		assertGone(resource.NewMetadata(omniresources.DefaultNamespace, omnires.ConfigPatchType, patchID, resource.VersionUndefined), "ConfigPatch "+patchID)
		assertGone(resource.NewMetadata(omniresources.DefaultNamespace, omnires.KernelArgsType, s.machineID, resource.VersionUndefined), "KernelArgs "+s.machineID)
		assertGone(resource.NewMetadata(omniresources.DefaultNamespace, omnires.ExtensionsConfigurationType, "schematic-"+s.setID, resource.VersionUndefined), "ExtensionsConfiguration schematic-"+s.setID)
		assertGone(resource.NewMetadata(omniresources.DefaultNamespace, omnires.MachineSetType, s.setID, resource.VersionUndefined), "MachineSet "+s.setID)
	}
	assertGone(resource.NewMetadata(omniresources.DefaultNamespace, omnires.ClusterType, clusterName, resource.VersionUndefined), "Cluster "+clusterName)
}

// ── pruneOrphanedMachineSets tests ────────────────────────────────────────────

// setupMachineSetWithResources creates a MachineSet with one bound machine plus
// the full per-machine/per-set resource complement: a ConfigPatch and
// KernelArgs for the machine, and an ExtensionsConfiguration for the set.
func setupMachineSetWithResources(t *testing.T, ctx context.Context, c *OmniClient, clusterName, machineSetID, machineID, role string) {
	t.Helper()
	if err := c.EnsureMachineSet(ctx, clusterName, machineSetID, role); err != nil {
		t.Fatalf("ensure machine set %s: %v", machineSetID, err)
	}
	setupAllocatedMachine(t, ctx, c, clusterName, machineSetID, machineID, role)
	patchID := fmt.Sprintf("%s-%s-%s", clusterName, machineID, "install-disk")
	inline := json.RawMessage(mustJSON(map[string]any{"machine": map[string]any{"install": map[string]any{"disk": "/dev/sda"}}}))
	if err := c.EnsureConfigPatch(ctx, patchID, clusterName, machineSetID, machineID, inline); err != nil {
		t.Fatalf("ensure config patch %s: %v", patchID, err)
	}
	if err := c.EnsureMachineKernelArgs(ctx, machineID, []string{"libata.force=noncq"}); err != nil {
		t.Fatalf("ensure kernel args %s: %v", machineID, err)
	}
	if err := c.EnsureMachineSetExtensionsConfiguration(ctx, clusterName, machineSetID, []string{"siderolabs/iscsi-tools"}); err != nil {
		t.Fatalf("ensure extensions configuration %s: %v", machineSetID, err)
	}
}

// TestPruneOrphanedMachineSets_ReleasesRemovedWorkerSet verifies that a worker
// machine set removed from spec.workers is torn down in Omni — its
// MachineSetNodes, ConfigPatches, KernelArgs, ExtensionsConfiguration, and the
// MachineSet itself — while a still-declared set and the control-plane set are
// left untouched.
func TestPruneOrphanedMachineSets_ReleasesRemovedWorkerSet(t *testing.T) {
	ctx := context.Background()
	st := newTestState()
	c := newTestOmniClient(st)

	clusterName := "test-cluster"
	cpSetID := omnires.ControlPlanesResourceID(clusterName)
	keepSetID := clusterName + "-workers-keep"
	orphanSetID := clusterName + "-workers-orphan"

	setupMachineSetWithResources(t, ctx, c, clusterName, cpSetID, "cp-uuid-1", omnires.LabelControlPlaneRole)
	setupMachineSetWithResources(t, ctx, c, clusterName, keepSetID, "worker-uuid-keep", omnires.LabelWorkerRole)
	setupMachineSetWithResources(t, ctx, c, clusterName, orphanSetID, "worker-uuid-orphan", omnires.LabelWorkerRole)

	r := newAllocReconciler(t, c)

	// Spec declares only the "workers-keep" set; "workers-orphan" was removed.
	cluster := &api.OmniCluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName},
		Spec: api.OmniClusterSpec{
			Workers: []api.WorkerMachineSetSpec{
				{Name: "workers-keep", MachineSetSpec: api.MachineSetSpec{Replicas: 1}},
			},
		},
	}

	if err := r.pruneOrphanedMachineSets(ctx, cluster, clusterName); err != nil {
		t.Fatalf("pruneOrphanedMachineSets: %v", err)
	}

	assertGone := func(md resource.Metadata, desc string) {
		t.Helper()
		_, err := st.Get(ctx, md)
		if err == nil {
			t.Errorf("%s should have been pruned but still exists", desc)
		} else if !state.IsNotFoundError(err) {
			t.Fatalf("unexpected error checking %s: %v", desc, err)
		}
	}
	assertPresent := func(md resource.Metadata, desc string) {
		t.Helper()
		if _, err := st.Get(ctx, md); err != nil {
			t.Errorf("%s should have survived pruning: %v", desc, err)
		}
	}

	// The orphaned set and all its resources are gone.
	orphanPatchID := fmt.Sprintf("%s-%s-%s", clusterName, "worker-uuid-orphan", "install-disk")
	assertGone(resource.NewMetadata(omniresources.DefaultNamespace, omnires.MachineSetNodeType, "worker-uuid-orphan", resource.VersionUndefined), "MachineSetNode worker-uuid-orphan")
	assertGone(resource.NewMetadata(omniresources.DefaultNamespace, omnires.ConfigPatchType, orphanPatchID, resource.VersionUndefined), "ConfigPatch "+orphanPatchID)
	assertGone(resource.NewMetadata(omniresources.DefaultNamespace, omnires.KernelArgsType, "worker-uuid-orphan", resource.VersionUndefined), "KernelArgs worker-uuid-orphan")
	assertGone(resource.NewMetadata(omniresources.DefaultNamespace, omnires.ExtensionsConfigurationType, "schematic-"+orphanSetID, resource.VersionUndefined), "ExtensionsConfiguration schematic-"+orphanSetID)
	assertGone(resource.NewMetadata(omniresources.DefaultNamespace, omnires.MachineSetType, orphanSetID, resource.VersionUndefined), "MachineSet "+orphanSetID)

	// The surviving worker set and the control-plane set are untouched.
	for _, s := range []struct{ setID, machineID string }{
		{keepSetID, "worker-uuid-keep"},
		{cpSetID, "cp-uuid-1"},
	} {
		patchID := fmt.Sprintf("%s-%s-%s", clusterName, s.machineID, "install-disk")
		assertPresent(resource.NewMetadata(omniresources.DefaultNamespace, omnires.MachineSetNodeType, s.machineID, resource.VersionUndefined), "MachineSetNode "+s.machineID)
		assertPresent(resource.NewMetadata(omniresources.DefaultNamespace, omnires.ConfigPatchType, patchID, resource.VersionUndefined), "ConfigPatch "+patchID)
		assertPresent(resource.NewMetadata(omniresources.DefaultNamespace, omnires.KernelArgsType, s.machineID, resource.VersionUndefined), "KernelArgs "+s.machineID)
		assertPresent(resource.NewMetadata(omniresources.DefaultNamespace, omnires.ExtensionsConfigurationType, "schematic-"+s.setID, resource.VersionUndefined), "ExtensionsConfiguration schematic-"+s.setID)
		assertPresent(resource.NewMetadata(omniresources.DefaultNamespace, omnires.MachineSetType, s.setID, resource.VersionUndefined), "MachineSet "+s.setID)
	}
}

// TestPruneOrphanedMachineSets_NeverPrunesControlPlane verifies that the
// control-plane machine set is never selected for pruning, even when the spec
// declares no workers at all.
func TestPruneOrphanedMachineSets_NeverPrunesControlPlane(t *testing.T) {
	ctx := context.Background()
	st := newTestState()
	c := newTestOmniClient(st)

	clusterName := "test-cluster"
	cpSetID := omnires.ControlPlanesResourceID(clusterName)

	setupMachineSetWithResources(t, ctx, c, clusterName, cpSetID, "cp-uuid-1", omnires.LabelControlPlaneRole)

	r := newAllocReconciler(t, c)
	cluster := &api.OmniCluster{ObjectMeta: metav1.ObjectMeta{Name: clusterName}}

	if err := r.pruneOrphanedMachineSets(ctx, cluster, clusterName); err != nil {
		t.Fatalf("pruneOrphanedMachineSets: %v", err)
	}

	msMD := resource.NewMetadata(omniresources.DefaultNamespace, omnires.MachineSetType, cpSetID, resource.VersionUndefined)
	if _, err := st.Get(ctx, msMD); err != nil {
		t.Errorf("control-plane MachineSet should never be pruned: %v", err)
	}
	nodeMD := resource.NewMetadata(omniresources.DefaultNamespace, omnires.MachineSetNodeType, "cp-uuid-1", resource.VersionUndefined)
	if _, err := st.Get(ctx, nodeMD); err != nil {
		t.Errorf("control-plane MachineSetNode should never be pruned: %v", err)
	}
}

// ── EnsureCluster ownership tests ─────────────────────────────────────────────

// TestEnsureCluster_SameOwnerUpdates verifies that the CR that created an Omni
// cluster can keep reconciling it: repeat calls with the same owner succeed
// and version changes propagate.
func TestEnsureCluster_SameOwnerUpdates(t *testing.T) {
	ctx := context.Background()
	st := newTestState()
	c := newTestOmniClient(st)

	clusterName := "prod"
	owner := "ns-a/prod"

	if err := c.EnsureCluster(ctx, clusterName, owner, "v1.30.0", "v1.8.0"); err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	if err := c.EnsureCluster(ctx, clusterName, owner, "v1.31.0", "v1.9.0"); err != nil {
		t.Fatalf("update cluster with same owner: %v", err)
	}

	md := resource.NewMetadata(omniresources.DefaultNamespace, omnires.ClusterType, clusterName, resource.VersionUndefined)
	stored, err := safe.StateGet[*omnires.Cluster](ctx, st, md)
	if err != nil {
		t.Fatalf("read back cluster: %v", err)
	}
	if got := stored.TypedSpec().Value.KubernetesVersion; got != "v1.31.0" {
		t.Errorf("kubernetes version not updated: got %s, want v1.31.0", got)
	}
	if got := stored.TypedSpec().Value.TalosVersion; got != "v1.9.0" {
		t.Errorf("talos version not updated: got %s, want v1.9.0", got)
	}
	if got, ok := stored.Metadata().Labels().Get(ownerLabel); !ok || got != owner {
		t.Errorf("owner label = %q (present=%v), want %q", got, ok, owner)
	}
}

// TestEnsureCluster_DifferentOwnerRefused verifies that a second OmniCluster CR
// with the same name in another namespace cannot adopt — or modify — an Omni
// cluster owned by the first CR.
func TestEnsureCluster_DifferentOwnerRefused(t *testing.T) {
	ctx := context.Background()
	st := newTestState()
	c := newTestOmniClient(st)

	clusterName := "prod"

	if err := c.EnsureCluster(ctx, clusterName, "ns-a/prod", "v1.30.0", "v1.8.0"); err != nil {
		t.Fatalf("create cluster: %v", err)
	}

	err := c.EnsureCluster(ctx, clusterName, "ns-b/prod", "v1.31.0", "v1.9.0")
	if err == nil {
		t.Fatal("expected ownership conflict error, got nil")
	}
	if !errors.Is(err, ErrClusterOwnershipConflict) {
		t.Errorf("expected error wrapping ErrClusterOwnershipConflict, got: %v", err)
	}

	// The foreign CR's versions must not have been applied.
	md := resource.NewMetadata(omniresources.DefaultNamespace, omnires.ClusterType, clusterName, resource.VersionUndefined)
	stored, getErr := safe.StateGet[*omnires.Cluster](ctx, st, md)
	if getErr != nil {
		t.Fatalf("read back cluster: %v", getErr)
	}
	if got := stored.TypedSpec().Value.KubernetesVersion; got != "v1.30.0" {
		t.Errorf("kubernetes version changed by non-owner: got %s, want v1.30.0", got)
	}
	if got, _ := stored.Metadata().Labels().Get(ownerLabel); got != "ns-a/prod" {
		t.Errorf("owner label changed by non-owner: got %q, want ns-a/prod", got)
	}
}

// TestEnsureCluster_AdoptsUnlabelledCluster verifies that an Omni cluster with
// no owner label (pre-dating the controller or created by hand) is adopted and
// stamped with the caller's owner identity.
func TestEnsureCluster_AdoptsUnlabelledCluster(t *testing.T) {
	ctx := context.Background()
	st := newTestState()
	c := newTestOmniClient(st)

	clusterName := "prod"
	owner := "ns-a/prod"

	preexisting := omnires.NewCluster(clusterName)
	preexisting.TypedSpec().Value.KubernetesVersion = "v1.30.0"
	preexisting.TypedSpec().Value.TalosVersion = "v1.8.0"
	if err := st.Create(ctx, preexisting); err != nil {
		t.Fatalf("pre-create unlabelled cluster: %v", err)
	}

	if err := c.EnsureCluster(ctx, clusterName, owner, "v1.30.0", "v1.8.0"); err != nil {
		t.Fatalf("EnsureCluster should adopt unlabelled cluster: %v", err)
	}

	md := resource.NewMetadata(omniresources.DefaultNamespace, omnires.ClusterType, clusterName, resource.VersionUndefined)
	stored, err := safe.StateGet[*omnires.Cluster](ctx, st, md)
	if err != nil {
		t.Fatalf("read back cluster: %v", err)
	}
	if got, ok := stored.Metadata().Labels().Get(ownerLabel); !ok || got != owner {
		t.Errorf("owner label after adoption = %q (present=%v), want %q", got, ok, owner)
	}
}

// TestDeleteOmniResources_SkipsForeignOwnedCluster verifies that deleting an
// OmniCluster CR does not tear down an Omni cluster owned by a different CR
// with the same name in another namespace.
func TestDeleteOmniResources_SkipsForeignOwnedCluster(t *testing.T) {
	ctx := context.Background()
	st := newTestState()
	c := newTestOmniClient(st)

	clusterName := "prod"

	if err := c.EnsureCluster(ctx, clusterName, "ns-a/prod", "v1.30.0", "v1.8.0"); err != nil {
		t.Fatalf("create cluster: %v", err)
	}

	r := &OmniClusterReconciler{OmniClient: c}
	foreignCR := &api.OmniCluster{ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: "ns-b"}}

	if err := r.deleteOmniResources(ctx, foreignCR); err != nil {
		t.Fatalf("deleteOmniResources should skip foreign-owned cluster without error: %v", err)
	}

	md := resource.NewMetadata(omniresources.DefaultNamespace, omnires.ClusterType, clusterName, resource.VersionUndefined)
	if _, err := safe.StateGet[*omnires.Cluster](ctx, st, md); err != nil {
		t.Errorf("foreign-owned cluster should still exist after skipped teardown: %v", err)
	}
}

// ── GetDriftingMachines tests ──────────────────────────────────────────────────

// createClusterMachineStatus inserts a ClusterMachineStatus fixture into the
// in-memory state, labelled with the cluster.
func createClusterMachineStatus(ctx context.Context, t *testing.T, st state.State, clusterName, machineID string, stage omniSpecs.ClusterMachineStatusSpec_Stage, ready, configUpToDate bool) {
	t.Helper()
	cms := omnires.NewClusterMachineStatus(machineID)
	cms.Metadata().Labels().Set(omnires.LabelCluster, clusterName)
	spec := cms.TypedSpec().Value
	spec.Stage = stage
	spec.Ready = ready
	spec.ConfigUpToDate = configUpToDate
	spec.ManagementAddress = "10.0.0.1"
	if err := st.Create(ctx, cms); err != nil {
		t.Fatalf("create ClusterMachineStatus %s: %v", machineID, err)
	}
}

// TestGetDriftingMachines_AllHealthyOneDrifting verifies that when every
// machine in the cluster is RUNNING+Ready, a drifting machine is returned
// as a reboot candidate.
func TestGetDriftingMachines_AllHealthyOneDrifting(t *testing.T) {
	ctx := context.Background()
	st := newTestState()
	c := newTestOmniClient(st)

	clusterName := "test-cluster"
	createClusterMachineStatus(ctx, t, st, clusterName, "machine-a", omniSpecs.ClusterMachineStatusSpec_RUNNING, true, false)
	createClusterMachineStatus(ctx, t, st, clusterName, "machine-b", omniSpecs.ClusterMachineStatusSpec_RUNNING, true, true)
	createClusterMachineStatus(ctx, t, st, clusterName, "machine-c", omniSpecs.ClusterMachineStatusSpec_RUNNING, true, true)

	drifting, err := c.GetDriftingMachines(ctx, clusterName)
	if err != nil {
		t.Fatalf("GetDriftingMachines: %v", err)
	}

	if len(drifting) != 1 {
		t.Fatalf("expected 1 drifting machine, got %d: %v", len(drifting), drifting)
	}
	if drifting[0].MachineID != "machine-a" {
		t.Errorf("expected drifting machine machine-a, got %s", drifting[0].MachineID)
	}
}

// TestGetDriftingMachines_UnhealthyMachineBlocksReboots verifies the
// cluster-wide health gate: if any machine is not RUNNING+Ready (e.g. still
// mid-reboot from a previous drift correction), no reboot candidates are
// returned even when another healthy machine is drifting.
func TestGetDriftingMachines_UnhealthyMachineBlocksReboots(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name  string
		stage omniSpecs.ClusterMachineStatusSpec_Stage
		ready bool
	}{
		{name: "non-running stage", stage: omniSpecs.ClusterMachineStatusSpec_REBOOTING, ready: false},
		{name: "running but not ready", stage: omniSpecs.ClusterMachineStatusSpec_RUNNING, ready: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestState()
			c := newTestOmniClient(st)

			clusterName := "test-cluster"
			// machine-a is unhealthy (e.g. just rebooted by the previous cycle).
			createClusterMachineStatus(ctx, t, st, clusterName, "machine-a", tc.stage, tc.ready, true)
			// machine-b is healthy and drifting.
			createClusterMachineStatus(ctx, t, st, clusterName, "machine-b", omniSpecs.ClusterMachineStatusSpec_RUNNING, true, false)
			createClusterMachineStatus(ctx, t, st, clusterName, "machine-c", omniSpecs.ClusterMachineStatusSpec_RUNNING, true, true)

			drifting, err := c.GetDriftingMachines(ctx, clusterName)
			if err != nil {
				t.Fatalf("GetDriftingMachines: %v", err)
			}
			if len(drifting) != 0 {
				t.Errorf("expected no reboot candidates while a machine is unhealthy, got %v", drifting)
			}
		})
	}
}

// ── selectDriftRebootCandidate / anyRebootInFlight tests ──────────────────────

// machineWithLastReboot builds a Machine with the given last-reboot time (nil for
// none), for exercising cooldown selection.
func machineWithLastReboot(id string, last *metav1.Time) *api.Machine {
	return &api.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: id},
		Status:     api.MachineStatus{LastRebootTime: last},
	}
}

func TestSelectDriftRebootCandidate(t *testing.T) {
	now := time.Now()
	machineA := MachineConfigDrift{MachineID: "machine-a", ManagementAddress: "10.0.0.1"}
	machineB := MachineConfigDrift{MachineID: "machine-b", ManagementAddress: "10.0.0.2"}
	recent := metav1.NewTime(now.Add(-1 * time.Minute))
	old := metav1.NewTime(now.Add(-rebootCooldown - time.Second))

	cases := []struct {
		name     string
		drifting []MachineConfigDrift
		byID     map[string]*api.Machine
		want     string // MachineID, "" for nil
	}{
		{
			name:     "no prior reboot returns first drifting machine",
			drifting: []MachineConfigDrift{machineA, machineB},
			byID:     map[string]*api.Machine{"machine-a": machineWithLastReboot("machine-a", nil), "machine-b": machineWithLastReboot("machine-b", nil)},
			want:     "machine-a",
		},
		{
			name:     "machine in cooldown is skipped for one past cooldown",
			drifting: []MachineConfigDrift{machineA, machineB},
			byID:     map[string]*api.Machine{"machine-a": machineWithLastReboot("machine-a", &recent), "machine-b": machineWithLastReboot("machine-b", nil)},
			want:     "machine-b",
		},
		{
			name:     "all within cooldown returns nil",
			drifting: []MachineConfigDrift{machineA, machineB},
			byID:     map[string]*api.Machine{"machine-a": machineWithLastReboot("machine-a", &recent), "machine-b": machineWithLastReboot("machine-b", &recent)},
			want:     "",
		},
		{
			name:     "reboot older than cooldown makes machine eligible again",
			drifting: []MachineConfigDrift{machineA},
			byID:     map[string]*api.Machine{"machine-a": machineWithLastReboot("machine-a", &old)},
			want:     "machine-a",
		},
		{
			name:     "drifting machine with no Machine object is skipped",
			drifting: []MachineConfigDrift{machineA},
			byID:     map[string]*api.Machine{},
			want:     "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectDriftRebootCandidate(tc.drifting, tc.byID, now)
			switch {
			case tc.want == "" && got != nil:
				t.Errorf("expected no candidate, got %v", got)
			case tc.want != "" && got == nil:
				t.Errorf("expected candidate %s, got nil", tc.want)
			case tc.want != "" && got.MachineID != tc.want:
				t.Errorf("expected candidate %s, got %s", tc.want, got.MachineID)
			}
		})
	}
}

// TestAnyRebootInFlight verifies the "one reboot in flight cluster-wide" gate:
// a machine whose Spec.RebootRequestedAt is newer than Status.LastRebootTime is
// owed a reboot and counts as in flight.
func TestAnyRebootInFlight(t *testing.T) {
	req := metav1.NewTime(time.Now())
	older := metav1.NewTime(req.Add(-time.Hour))
	newer := metav1.NewTime(req.Add(time.Hour))

	mk := func(reqAt, last *metav1.Time) api.Machine {
		return api.Machine{
			Spec:   api.MachineSpec{RebootRequestedAt: reqAt},
			Status: api.MachineStatus{LastRebootTime: last},
		}
	}

	cases := []struct {
		name     string
		machines []api.Machine
		want     bool
	}{
		{"none requested", []api.Machine{mk(nil, nil)}, false},
		{"requested, never rebooted", []api.Machine{mk(&req, nil)}, true},
		{"requested newer than last reboot", []api.Machine{mk(&req, &older)}, true},
		{"already rebooted after request", []api.Machine{mk(&req, &newer)}, false},
		{"one of several in flight", []api.Machine{mk(nil, nil), mk(&req, &older)}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := anyRebootInFlight(tc.machines); got != tc.want {
				t.Errorf("anyRebootInFlight = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSignalDriftReboots_RequestsOneCandidate verifies that the cluster
// controller signals a reboot by stamping Spec.RebootRequestedAt (and the
// management address) on exactly one drifting machine, and that a reboot already
// in flight suppresses a second request.
func TestSignalDriftReboots_RequestsOneCandidate(t *testing.T) {
	ctx := context.Background()
	c := newTestOmniClient(newTestState())

	clusterName := "test-cluster"
	setID := "test-cluster-workers"
	mA := newAllocatedMachine(clusterName, setID, roleWorker, "machine-a")
	mB := newAllocatedMachine(clusterName, setID, roleWorker, "machine-b")
	r := newAllocReconciler(t, c, mA, mB)
	cluster := &api.OmniCluster{ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: "default"}}

	drifting := []MachineConfigDrift{
		{MachineID: "machine-a", ManagementAddress: "10.0.0.1"},
		{MachineID: "machine-b", ManagementAddress: "10.0.0.2"},
	}
	if err := r.signalDriftReboots(ctx, cluster, clusterName, drifting); err != nil {
		t.Fatalf("signalDriftReboots: %v", err)
	}

	requested := 0
	list := &api.MachineList{}
	if err := r.List(ctx, list); err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, m := range list.Items {
		if m.Spec.RebootRequestedAt != nil {
			requested++
			if m.Spec.ManagementAddress == "" {
				t.Errorf("machine %s requested reboot without management address", m.Name)
			}
		}
	}
	if requested != 1 {
		t.Fatalf("expected exactly 1 machine with a reboot requested, got %d", requested)
	}

	// A second pass while one reboot is in flight must not request another.
	if err := r.signalDriftReboots(ctx, cluster, clusterName, drifting); err != nil {
		t.Fatalf("second signalDriftReboots: %v", err)
	}
	requested = 0
	list = &api.MachineList{}
	if err := r.List(ctx, list); err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, m := range list.Items {
		if m.Spec.RebootRequestedAt != nil {
			requested++
		}
	}
	if requested != 1 {
		t.Errorf("expected still exactly 1 reboot in flight, got %d", requested)
	}
}

// ── statusNeedsUpdate tests ───────────────────────────────────────────────────

// TestStatusNeedsUpdate verifies the conditional-update decision that breaks
// the watch-driven hot loop: re-deriving an identical status (including
// conditions re-set via meta.SetStatusCondition) must not trigger a write,
// while any real change must.
func TestStatusNeedsUpdate(t *testing.T) {
	transition := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	baseStatus := func() *api.OmniClusterStatus {
		return &api.OmniClusterStatus{
			Phase: "Running",
			Ready: true,
			AllocatedMachines: map[string][]string{
				"cp": {"machine-a"},
			},
			Conditions: []metav1.Condition{{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				Reason:             "Running",
				Message:            "Omni cluster phase: Running",
				LastTransitionTime: transition,
			}},
		}
	}

	t.Run("identical status needs no update", func(t *testing.T) {
		if statusNeedsUpdate(baseStatus(), baseStatus()) {
			t.Error("expected no update for identical statuses")
		}
	})

	t.Run("re-setting an unchanged condition needs no update", func(t *testing.T) {
		before := baseStatus()
		after := before.DeepCopy()
		meta.SetStatusCondition(&after.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionTrue,
			Reason:  "Running",
			Message: "Omni cluster phase: Running",
		})
		if statusNeedsUpdate(before, after) {
			t.Error("expected no update when condition is re-set with identical values")
		}
		if !after.Conditions[0].LastTransitionTime.Equal(&transition) {
			t.Errorf("LastTransitionTime changed without a transition: %v", after.Conditions[0].LastTransitionTime)
		}
	})

	t.Run("condition transition needs update", func(t *testing.T) {
		before := baseStatus()
		after := before.DeepCopy()
		meta.SetStatusCondition(&after.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionFalse,
			Reason:  "ScalingUp",
			Message: "Omni cluster phase: ScalingUp",
		})
		if !statusNeedsUpdate(before, after) {
			t.Error("expected update when condition status transitions")
		}
		if after.Conditions[0].LastTransitionTime.Equal(&transition) {
			t.Error("LastTransitionTime should be re-stamped on a transition")
		}
	})

	t.Run("field change needs update", func(t *testing.T) {
		before := baseStatus()
		after := before.DeepCopy()
		after.Phase = "Destroying"
		if !statusNeedsUpdate(before, after) {
			t.Error("expected update when a status field changes")
		}
	})

	t.Run("observedGeneration bump alone needs update", func(t *testing.T) {
		before := baseStatus()
		after := before.DeepCopy()
		after.ObservedGeneration = before.ObservedGeneration + 1
		if !statusNeedsUpdate(before, after) {
			t.Error("expected update when observedGeneration changes")
		}
	})
}

// ── secretNeedsRefresh tests ───────────────────────────────────────────────────

func TestSecretNeedsRefresh(t *testing.T) {
	now := time.Now()

	newSecret := func(data map[string][]byte, annotations map[string]string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Annotations: annotations},
			Data:       data,
		}
	}

	t.Run("nil secret needs refresh", func(t *testing.T) {
		if !secretNeedsRefresh(nil, "value", now) {
			t.Error("expected nil secret to need refresh")
		}
	})

	t.Run("missing data key needs refresh", func(t *testing.T) {
		secret := newSecret(map[string][]byte{"other": []byte("x")}, map[string]string{
			kubeconfigRefreshedAtAnnotation: now.Format(time.RFC3339),
		})
		if !secretNeedsRefresh(secret, "value", now) {
			t.Error("expected secret without data key to need refresh")
		}
	})

	t.Run("missing annotation needs refresh", func(t *testing.T) {
		secret := newSecret(map[string][]byte{"value": []byte("x")}, nil)
		if !secretNeedsRefresh(secret, "value", now) {
			t.Error("expected secret without refresh annotation to need refresh")
		}
	})

	t.Run("fresh annotation does not need refresh", func(t *testing.T) {
		secret := newSecret(map[string][]byte{"value": []byte("x")}, map[string]string{
			kubeconfigRefreshedAtAnnotation: now.Add(-time.Hour).Format(time.RFC3339),
		})
		if secretNeedsRefresh(secret, "value", now) {
			t.Error("expected 1-hour-old secret to not need refresh")
		}
	})

	t.Run("annotation older than interval needs refresh", func(t *testing.T) {
		secret := newSecret(map[string][]byte{"value": []byte("x")}, map[string]string{
			kubeconfigRefreshedAtAnnotation: now.Add(-kubeconfigRefreshInterval - time.Hour).Format(time.RFC3339),
		})
		if !secretNeedsRefresh(secret, "value", now) {
			t.Error("expected secret older than refresh interval to need refresh")
		}
	})

	t.Run("unparsable annotation needs refresh", func(t *testing.T) {
		secret := newSecret(map[string][]byte{"value": []byte("x")}, map[string]string{
			kubeconfigRefreshedAtAnnotation: "not-a-timestamp",
		})
		if !secretNeedsRefresh(secret, "value", now) {
			t.Error("expected secret with unparsable annotation to need refresh")
		}
	})
}

// ── EnsureMachineSetNode conflict tests ───────────────────────────────────────

// TestEnsureMachineSetNode_IdempotentSameSet verifies that re-binding a machine
// to the machine set it is already bound to succeeds without error.
func TestEnsureMachineSetNode_IdempotentSameSet(t *testing.T) {
	ctx := context.Background()
	st := newTestState()
	c := newTestOmniClient(st)

	clusterName := "cluster-a"
	machineSetID := "cluster-a-workers"
	machineID := "m1"

	if err := c.EnsureMachineSetNode(ctx, clusterName, machineSetID, machineID, omnires.LabelWorkerRole); err != nil {
		t.Fatalf("first EnsureMachineSetNode: %v", err)
	}
	if err := c.EnsureMachineSetNode(ctx, clusterName, machineSetID, machineID, omnires.LabelWorkerRole); err != nil {
		t.Errorf("second EnsureMachineSetNode for the same set should be idempotent, got: %v", err)
	}
}

// TestEnsureMachineSetNode_ConflictDifferentSet verifies that binding a machine
// already owned by another machine set fails loudly instead of silently
// counting the machine as allocated to the new set.
func TestEnsureMachineSetNode_ConflictDifferentSet(t *testing.T) {
	ctx := context.Background()
	st := newTestState()
	c := newTestOmniClient(st)

	machineID := "m1"

	if err := c.EnsureMachineSetNode(ctx, "cluster-a", "cluster-a-workers", machineID, omnires.LabelWorkerRole); err != nil {
		t.Fatalf("bind to cluster-a-workers: %v", err)
	}

	err := c.EnsureMachineSetNode(ctx, "cluster-b", "cluster-b-workers", machineID, omnires.LabelWorkerRole)
	if err == nil {
		t.Fatal("expected error binding machine already owned by cluster-a-workers, got nil")
	}
	if !strings.Contains(err.Error(), "cluster-a-workers") {
		t.Errorf("error should name the conflicting machine set cluster-a-workers, got: %v", err)
	}
}

// ── SelectAvailableMachines tests ─────────────────────────────────────────────

// createMachineStatus inserts a MachineStatus fixture into the in-memory state
// with the given labels. available controls the built-in availability label.
func createMachineStatus(ctx context.Context, t *testing.T, st state.State, id string, available bool, machineLabels map[string]string) {
	t.Helper()
	ms := omnires.NewMachineStatus(id)
	if available {
		ms.Metadata().Labels().Set(omnires.MachineStatusLabelAvailable, "")
	}
	for k, v := range machineLabels {
		ms.Metadata().Labels().Set(k, v)
	}
	if err := st.Create(ctx, ms); err != nil {
		t.Fatalf("create MachineStatus %s: %v", id, err)
	}
}

// TestSelectAvailableMachines_MatchExpressions verifies that machineSelector
// matchExpressions are honoured with standard Kubernetes label-selector
// semantics, including the missing-key behaviour of NotIn and DoesNotExist.
func TestSelectAvailableMachines_MatchExpressions(t *testing.T) {
	ctx := context.Background()

	// Fixture fleet:
	//   metal-small   platform=metal size=small
	//   metal-large   platform=metal size=large
	//   aws-large     platform=aws   size=large
	//   unlabelled    (no platform/size labels)
	//   taken         platform=metal, but NOT available
	setup := func(t *testing.T) *OmniClient {
		st := newTestState()
		createMachineStatus(ctx, t, st, "metal-small", true, map[string]string{"platform": "metal", "size": "small"})
		createMachineStatus(ctx, t, st, "metal-large", true, map[string]string{"platform": "metal", "size": "large"})
		createMachineStatus(ctx, t, st, "aws-large", true, map[string]string{"platform": "aws", "size": "large"})
		createMachineStatus(ctx, t, st, "unlabelled", true, nil)
		createMachineStatus(ctx, t, st, "taken", false, map[string]string{"platform": "metal", "size": "large"})
		return newTestOmniClient(st)
	}

	cases := []struct {
		name string
		sel  metav1.LabelSelector
		want []string
	}{
		{
			name: "Exists selects only machines with the key",
			sel: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "platform", Operator: metav1.LabelSelectorOpExists},
			}},
			want: []string{"aws-large", "metal-large", "metal-small"},
		},
		{
			name: "DoesNotExist excludes machines with the key",
			sel: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "platform", Operator: metav1.LabelSelectorOpDoesNotExist},
			}},
			want: []string{"unlabelled"},
		},
		{
			name: "In with two values selects exactly the matching machines",
			sel: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "platform", Operator: metav1.LabelSelectorOpIn, Values: []string{"metal", "aws"}},
			}},
			want: []string{"aws-large", "metal-large", "metal-small"},
		},
		{
			name: "NotIn excludes listed values but includes machines lacking the key",
			sel: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "platform", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"aws"}},
			}},
			want: []string{"metal-large", "metal-small", "unlabelled"},
		},
		{
			name: "matchLabels and matchExpressions AND together",
			sel: metav1.LabelSelector{
				MatchLabels: map[string]string{"platform": "metal"},
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "size", Operator: metav1.LabelSelectorOpIn, Values: []string{"large"}},
				},
			},
			want: []string{"metal-large"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := setup(t)
			got, err := c.SelectAvailableMachines(ctx, tc.sel, 10)
			if err != nil {
				t.Fatalf("SelectAvailableMachines: %v", err)
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Errorf("selected machines mismatch:\n  got  %v\n  want %v", got, tc.want)
			}
		})
	}
}

// TestSelectAvailableMachines_InvalidSelector verifies that a selector that
// fails Kubernetes validation (In with no values) is rejected with an error.
func TestSelectAvailableMachines_InvalidSelector(t *testing.T) {
	ctx := context.Background()
	c := newTestOmniClient(newTestState())

	_, err := c.SelectAvailableMachines(ctx, metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "platform", Operator: metav1.LabelSelectorOpIn},
		},
	}, 10)
	if err == nil {
		t.Fatal("expected error for In expression without values, got nil")
	}
}

// ── teardownAndDestroy finalizer tests ────────────────────────────────────────

// TestDeleteExtensionsConfiguration_WithFinalizer is a regression test for the
// v0.2.0 orphan-prune failure: Omni's MachineExtensionsController holds a
// finalizer on ExtensionsConfiguration, so a bare Destroy fails with
// FailedPrecondition. Deletion must Teardown first, surface a retryable error
// while the finalizer is held, and complete once it is released.
func TestDeleteExtensionsConfiguration_WithFinalizer(t *testing.T) {
	ctx := context.Background()
	st := newTestState()
	c := newTestOmniClient(st)

	clusterName := "test-cluster"
	machineSetID := "test-cluster-gpu"

	if err := c.EnsureMachineSetExtensionsConfiguration(ctx, clusterName, machineSetID,
		[]string{"siderolabs/nvidia-open-gpu-kernel-modules"}); err != nil {
		t.Fatalf("create extensions configuration: %v", err)
	}

	// Simulate Omni's controller holding a finalizer.
	md := resource.NewMetadata(omniresources.DefaultNamespace, omnires.ExtensionsConfigurationType,
		"schematic-"+machineSetID, resource.VersionUndefined)
	if err := st.AddFinalizer(ctx, md, "MachineExtensionsController"); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}

	// First attempt: teardown starts, finalizer still held → retryable error.
	err := c.DeleteExtensionsConfigurationForMachineSet(ctx, machineSetID)
	if err == nil {
		t.Fatal("expected retryable error while finalizer is held, got nil")
	}

	// Omni's controller reacts to the teardown by releasing its finalizer.
	if err := st.RemoveFinalizer(ctx, md, "MachineExtensionsController"); err != nil {
		t.Fatalf("remove finalizer: %v", err)
	}

	// Retry (as controller-runtime would): deletion now completes.
	if err := c.DeleteExtensionsConfigurationForMachineSet(ctx, machineSetID); err != nil {
		t.Fatalf("delete after finalizer released: %v", err)
	}
	if _, err := safe.StateGet[*omnires.ExtensionsConfiguration](ctx, st, md); !state.IsNotFoundError(err) {
		t.Fatalf("extensions configuration should be gone, got err=%v", err)
	}

	// Idempotency: deleting again is a no-op.
	if err := c.DeleteExtensionsConfigurationForMachineSet(ctx, machineSetID); err != nil {
		t.Fatalf("delete of missing resource should be nil, got %v", err)
	}
}
