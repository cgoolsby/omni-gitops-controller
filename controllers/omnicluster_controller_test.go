package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// TestApplyConfigPatches_PrunesRemoved verifies that removing a ConfigPatch
// from the spec causes PruneOrphanedConfigPatches to delete it from Omni.
func TestApplyConfigPatches_PrunesRemoved(t *testing.T) {
	ctx := context.Background()
	st := newTestState()
	c := newTestOmniClient(st)

	clusterName := "test-cluster"
	machineSetID := "test-cluster-workers"
	machineID := "machine-uuid-3"
	patchName := "install-disk"
	patchID := fmt.Sprintf("%s-%s-%s", clusterName, machineID, patchName)

	r := &OmniClusterReconciler{OmniClient: c}

	// Apply one patch.
	patches := []api.ConfigPatch{
		{
			Name:   patchName,
			Inline: apiextensionsv1.JSON{Raw: mustJSON(map[string]any{"machine": map[string]any{"install": map[string]any{"disk": "/dev/sda"}}})},
		},
	}
	if err := r.applyConfigPatches(ctx, clusterName, machineSetID, machineID, patches); err != nil {
		t.Fatalf("apply patches: %v", err)
	}

	// Verify it exists.
	md := resource.NewMetadata(omniresources.DefaultNamespace, omnires.ConfigPatchType, patchID, resource.VersionUndefined)
	if _, err := safe.StateGet[*omnires.ConfigPatch](ctx, st, md); err != nil {
		t.Fatalf("patch should exist after apply: %v", err)
	}

	// Now apply an empty patch list (simulate user removing the patch from the CR).
	if err := r.applyConfigPatches(ctx, clusterName, machineSetID, machineID, nil); err != nil {
		t.Fatalf("apply empty patches: %v", err)
	}

	// Patch must be gone.
	_, err := safe.StateGet[*omnires.ConfigPatch](ctx, st, md)
	if err == nil {
		t.Errorf("config patch %s should have been pruned but still exists", patchID)
	} else if !state.IsNotFoundError(err) {
		t.Fatalf("unexpected error checking pruned patch: %v", err)
	}
}

// ── reconcileMachineSet prune tests ───────────────────────────────────────────

// setupAllocatedMachine pre-creates a MachineSetNode in the in-memory state so
// that AllocatedMachineSetNodes returns it without needing a real Omni server.
func setupAllocatedMachine(t *testing.T, ctx context.Context, c *OmniClient, clusterName, machineSetID, machineID, role string) {
	t.Helper()
	if err := c.EnsureMachineSetNode(ctx, clusterName, machineSetID, machineID, role); err != nil {
		t.Fatalf("pre-create MachineSetNode: %v", err)
	}
}

// TestReconcileMachineSet_PrunesKernelArgsWhenEmpty verifies that when
// spec.KernelArgs is cleared, the per-machine KernelArgs resource is deleted.
func TestReconcileMachineSet_PrunesKernelArgsWhenEmpty(t *testing.T) {
	ctx := context.Background()
	st := newTestState()
	c := newTestOmniClient(st)

	clusterName := "test-cluster"
	machineSetID := "test-cluster-workers"
	machineID := "machine-uuid-4"

	setupAllocatedMachine(t, ctx, c, clusterName, machineSetID, machineID, omnires.LabelWorkerRole)

	r := &OmniClusterReconciler{OmniClient: c}

	// Apply with kernelArgs set.
	specWithArgs := api.MachineSetSpec{
		Replicas:      1,
		KernelArgs:    []string{"libata.force=noncq"},
		ConfigPatches: nil,
	}
	if _, err := r.reconcileMachineSet(ctx, &api.OmniCluster{}, clusterName, machineSetID, omnires.LabelWorkerRole, specWithArgs); err != nil {
		t.Fatalf("reconcile with kernelArgs: %v", err)
	}

	// KernelArgs resource must exist.
	md := resource.NewMetadata(omniresources.DefaultNamespace, omnires.KernelArgsType, machineID, resource.VersionUndefined)
	if _, err := safe.StateGet[*omnires.KernelArgs](ctx, st, md); err != nil {
		t.Fatalf("KernelArgs resource should exist after reconcile with args: %v", err)
	}

	// Now reconcile with kernelArgs cleared.
	specEmpty := api.MachineSetSpec{
		Replicas:      1,
		KernelArgs:    nil,
		ConfigPatches: nil,
	}
	if _, err := r.reconcileMachineSet(ctx, &api.OmniCluster{}, clusterName, machineSetID, omnires.LabelWorkerRole, specEmpty); err != nil {
		t.Fatalf("reconcile with empty kernelArgs: %v", err)
	}

	// KernelArgs resource must be gone.
	_, err := safe.StateGet[*omnires.KernelArgs](ctx, st, md)
	if err == nil {
		t.Errorf("KernelArgs resource should have been deleted when spec.KernelArgs is empty")
	} else if !state.IsNotFoundError(err) {
		t.Fatalf("unexpected error checking deleted KernelArgs: %v", err)
	}
}

// TestReconcileMachineSet_EvictionDeletesKernelArgs verifies that scaling a
// machine set down deletes the evicted machine's KernelArgs resource, even
// when spec.KernelArgs is still set, so the machine returns to the available
// pool without custom kernel args.
func TestReconcileMachineSet_EvictionDeletesKernelArgs(t *testing.T) {
	ctx := context.Background()
	st := newTestState()
	c := newTestOmniClient(st)

	clusterName := "test-cluster"
	machineSetID := "test-cluster-workers"
	machineID := "machine-uuid-evict"

	setupAllocatedMachine(t, ctx, c, clusterName, machineSetID, machineID, omnires.LabelWorkerRole)

	r := &OmniClusterReconciler{OmniClient: c}

	spec := api.MachineSetSpec{
		Replicas:   1,
		KernelArgs: []string{"libata.force=noncq"},
	}
	if _, err := r.reconcileMachineSet(ctx, &api.OmniCluster{}, clusterName, machineSetID, omnires.LabelWorkerRole, spec); err != nil {
		t.Fatalf("reconcile with kernelArgs: %v", err)
	}

	// KernelArgs resource must exist.
	kaMD := resource.NewMetadata(omniresources.DefaultNamespace, omnires.KernelArgsType, machineID, resource.VersionUndefined)
	if _, err := safe.StateGet[*omnires.KernelArgs](ctx, st, kaMD); err != nil {
		t.Fatalf("KernelArgs resource should exist after reconcile with args: %v", err)
	}

	// Scale down to 0 while still passing the same KernelArgs in the spec, so
	// the deletion must come from the eviction path, not the prune-on-clear path.
	spec.Replicas = 0
	if _, err := r.reconcileMachineSet(ctx, &api.OmniCluster{}, clusterName, machineSetID, omnires.LabelWorkerRole, spec); err != nil {
		t.Fatalf("reconcile scale-down: %v", err)
	}

	// KernelArgs resource must be gone.
	if _, err := safe.StateGet[*omnires.KernelArgs](ctx, st, kaMD); err == nil {
		t.Errorf("KernelArgs resource should have been deleted for evicted machine %s", machineID)
	} else if !state.IsNotFoundError(err) {
		t.Fatalf("unexpected error checking deleted KernelArgs: %v", err)
	}

	// MachineSetNode must be gone.
	nodeMD := resource.NewMetadata(omniresources.DefaultNamespace, omnires.MachineSetNodeType, machineID, resource.VersionUndefined)
	if _, err := safe.StateGet[*omnires.MachineSetNode](ctx, st, nodeMD); err == nil {
		t.Errorf("MachineSetNode should have been deleted for evicted machine %s", machineID)
	} else if !state.IsNotFoundError(err) {
		t.Fatalf("unexpected error checking deleted MachineSetNode: %v", err)
	}
}

// TestReconcileMachineSet_PrunesExtensionsWhenEmpty verifies that when
// spec.MachineExtensions is cleared, the ExtensionsConfiguration resource
// for the machine set is deleted.
func TestReconcileMachineSet_PrunesExtensionsWhenEmpty(t *testing.T) {
	ctx := context.Background()
	st := newTestState()
	c := newTestOmniClient(st)

	clusterName := "test-cluster"
	machineSetID := "test-cluster-workers"
	machineID := "machine-uuid-5"

	setupAllocatedMachine(t, ctx, c, clusterName, machineSetID, machineID, omnires.LabelWorkerRole)

	r := &OmniClusterReconciler{OmniClient: c}

	// Apply with extensions set.
	specWithExts := api.MachineSetSpec{
		Replicas:          1,
		MachineExtensions: []string{"siderolabs/nvidia-open-gpu-kernel-modules"},
	}
	if _, err := r.reconcileMachineSet(ctx, &api.OmniCluster{}, clusterName, machineSetID, omnires.LabelWorkerRole, specWithExts); err != nil {
		t.Fatalf("reconcile with extensions: %v", err)
	}

	// ExtensionsConfiguration must exist.
	extID := "schematic-" + machineSetID
	md := resource.NewMetadata(omniresources.DefaultNamespace, omnires.ExtensionsConfigurationType, extID, resource.VersionUndefined)
	if _, err := safe.StateGet[*omnires.ExtensionsConfiguration](ctx, st, md); err != nil {
		t.Fatalf("ExtensionsConfiguration should exist after reconcile with extensions: %v", err)
	}

	// Now reconcile with extensions cleared.
	specEmpty := api.MachineSetSpec{
		Replicas:          1,
		MachineExtensions: nil,
	}
	if _, err := r.reconcileMachineSet(ctx, &api.OmniCluster{}, clusterName, machineSetID, omnires.LabelWorkerRole, specEmpty); err != nil {
		t.Fatalf("reconcile with empty extensions: %v", err)
	}

	// ExtensionsConfiguration must be gone.
	_, err := safe.StateGet[*omnires.ExtensionsConfiguration](ctx, st, md)
	if err == nil {
		t.Errorf("ExtensionsConfiguration should have been deleted when spec.MachineExtensions is empty")
	} else if !state.IsNotFoundError(err) {
		t.Fatalf("unexpected error checking deleted ExtensionsConfiguration: %v", err)
	}
}

// ── reconcileMachineSet scale-down tests ──────────────────────────────────────

// TestReconcileMachineSet_ControlPlaneScaleDownOneAtATime verifies that
// control-plane scale-down removes at most one member per reconcile, so a
// 3 → 1 scale takes two reconcile cycles instead of evicting two etcd
// members at once.
func TestReconcileMachineSet_ControlPlaneScaleDownOneAtATime(t *testing.T) {
	ctx := context.Background()
	c := newTestOmniClient(newTestState())

	clusterName := "test-cluster"
	machineSetID := "test-cluster-control-planes"

	for i := 1; i <= 3; i++ {
		setupAllocatedMachine(t, ctx, c, clusterName, machineSetID, fmt.Sprintf("cp-uuid-%d", i), omnires.LabelControlPlaneRole)
	}

	r := &OmniClusterReconciler{OmniClient: c}
	spec := api.MachineSetSpec{Replicas: 1}

	if _, err := r.reconcileMachineSet(ctx, &api.OmniCluster{}, clusterName, machineSetID, omnires.LabelControlPlaneRole, spec); err != nil {
		t.Fatalf("first scale-down reconcile: %v", err)
	}
	remaining, err := c.AllocatedMachineSetNodes(ctx, machineSetID)
	if err != nil {
		t.Fatalf("list allocated after first reconcile: %v", err)
	}
	if len(remaining) != 2 {
		t.Fatalf("after first reconcile: got %d machines, want 2 (one member removed per cycle)", len(remaining))
	}

	if _, err := r.reconcileMachineSet(ctx, &api.OmniCluster{}, clusterName, machineSetID, omnires.LabelControlPlaneRole, spec); err != nil {
		t.Fatalf("second scale-down reconcile: %v", err)
	}
	remaining, err = c.AllocatedMachineSetNodes(ctx, machineSetID)
	if err != nil {
		t.Fatalf("list allocated after second reconcile: %v", err)
	}
	if len(remaining) != 1 {
		t.Errorf("after second reconcile: got %d machines, want 1", len(remaining))
	}
}

// TestReconcileMachineSet_WorkerScaleDownBatch verifies that worker
// scale-down still removes all excess machines in a single reconcile.
func TestReconcileMachineSet_WorkerScaleDownBatch(t *testing.T) {
	ctx := context.Background()
	c := newTestOmniClient(newTestState())

	clusterName := "test-cluster"
	machineSetID := "test-cluster-workers"

	for i := 1; i <= 3; i++ {
		setupAllocatedMachine(t, ctx, c, clusterName, machineSetID, fmt.Sprintf("worker-uuid-%d", i), omnires.LabelWorkerRole)
	}

	r := &OmniClusterReconciler{OmniClient: c}
	spec := api.MachineSetSpec{Replicas: 1}

	if _, err := r.reconcileMachineSet(ctx, &api.OmniCluster{}, clusterName, machineSetID, omnires.LabelWorkerRole, spec); err != nil {
		t.Fatalf("scale-down reconcile: %v", err)
	}
	remaining, err := c.AllocatedMachineSetNodes(ctx, machineSetID)
	if err != nil {
		t.Fatalf("list allocated after reconcile: %v", err)
	}
	if len(remaining) != 1 {
		t.Errorf("after reconcile: got %d machines, want 1 (workers scale down in one batch)", len(remaining))
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

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}

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

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}
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

	r := &OmniClusterReconciler{OmniClient: c}

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

	r := &OmniClusterReconciler{OmniClient: c}
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

// ── selectRebootCandidate tests ───────────────────────────────────────────────

func TestSelectRebootCandidate(t *testing.T) {
	now := time.Now()
	machineA := MachineConfigDrift{MachineID: "machine-a", ManagementAddress: "10.0.0.1"}
	machineB := MachineConfigDrift{MachineID: "machine-b", ManagementAddress: "10.0.0.2"}

	cases := []struct {
		name        string
		drifting    []MachineConfigDrift
		lastReboots map[string]metav1.Time
		want        *MachineConfigDrift
	}{
		{
			name:        "no prior reboot recorded returns first drifting machine",
			drifting:    []MachineConfigDrift{machineA, machineB},
			lastReboots: nil,
			want:        &machineA,
		},
		{
			name:     "machine in cooldown is skipped in favor of one with no record",
			drifting: []MachineConfigDrift{machineA, machineB},
			lastReboots: map[string]metav1.Time{
				"machine-a": metav1.NewTime(now.Add(-1 * time.Minute)),
			},
			want: &machineB,
		},
		{
			name:     "all drifting machines within cooldown returns nil",
			drifting: []MachineConfigDrift{machineA, machineB},
			lastReboots: map[string]metav1.Time{
				"machine-a": metav1.NewTime(now.Add(-1 * time.Minute)),
				"machine-b": metav1.NewTime(now.Add(-9 * time.Minute)),
			},
			want: nil,
		},
		{
			name:     "reboot older than cooldown makes machine eligible again",
			drifting: []MachineConfigDrift{machineA},
			lastReboots: map[string]metav1.Time{
				"machine-a": metav1.NewTime(now.Add(-rebootCooldown - time.Second)),
			},
			want: &machineA,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectRebootCandidate(tc.drifting, tc.lastReboots, now)
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("expected no candidate, got %v", got)
			case tc.want != nil && got == nil:
				t.Errorf("expected candidate %s, got nil", tc.want.MachineID)
			case tc.want != nil && got.MachineID != tc.want.MachineID:
				t.Errorf("expected candidate %s, got %s", tc.want.MachineID, got.MachineID)
			}
		})
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
