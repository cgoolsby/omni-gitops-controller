package controllers

import (
	"context"
	"encoding/json"
	"fmt"
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
