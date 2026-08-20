package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	omniresources "github.com/siderolabs/omni/client/pkg/omni/resources"
	omnires "github.com/siderolabs/omni/client/pkg/omni/resources/omni"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/cgoolsby/omni-gitops-controller/api/v1alpha1"
)

// fakeMachineOmniClient records the per-machine Omni calls in order so tests can
// assert bind/config/kernel-args/reboot/teardown sequencing without a live Omni.
type fakeMachineOmniClient struct {
	calls         []string
	rebootErr     error
	ensureNodeErr error
	rebootedAddrs []string
}

func (f *fakeMachineOmniClient) EnsureMachineSetNode(_ context.Context, _, _, _, _ string) error {
	f.calls = append(f.calls, "bind")
	return f.ensureNodeErr
}
func (f *fakeMachineOmniClient) DeleteMachineSetNode(_ context.Context, _ string) error {
	f.calls = append(f.calls, "unbind")
	return nil
}
func (f *fakeMachineOmniClient) EnsureConfigPatch(_ context.Context, _, _, _, _ string, _ json.RawMessage) error {
	f.calls = append(f.calls, "ensureConfig")
	return nil
}
func (f *fakeMachineOmniClient) PruneOrphanedConfigPatches(_ context.Context, _, _ string, _ map[string]struct{}) error {
	f.calls = append(f.calls, "pruneConfig")
	return nil
}
func (f *fakeMachineOmniClient) DeleteConfigPatchesForMachine(_ context.Context, _, _ string) error {
	f.calls = append(f.calls, "deleteConfig")
	return nil
}
func (f *fakeMachineOmniClient) EnsureMachineKernelArgs(_ context.Context, _ string, _ []string) error {
	f.calls = append(f.calls, "ensureKernel")
	return nil
}
func (f *fakeMachineOmniClient) DeleteKernelArgsForMachine(_ context.Context, _ string) error {
	f.calls = append(f.calls, "deleteKernel")
	return nil
}
func (f *fakeMachineOmniClient) RebootMachine(_ context.Context, _, managementAddress string) error {
	f.calls = append(f.calls, "reboot")
	f.rebootedAddrs = append(f.rebootedAddrs, managementAddress)
	return f.rebootErr
}

var _ machineOmniClient = (*fakeMachineOmniClient)(nil)

// newMachineReconciler builds a MachineReconciler with a fake client holding the
// given Machine plus a status subresource, and the provided Omni client seam.
func newMachineReconciler(t *testing.T, oc machineOmniClient, machine *api.Machine) *MachineReconciler {
	t.Helper()
	scheme := newTestScheme(t)
	return &MachineReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(machine).
			WithStatusSubresource(&api.Machine{}).
			Build(),
		Scheme:     scheme,
		OmniClient: oc,
	}
}

func machineReq(m *api.Machine) reconcile.Request {
	return reconcile.Request{NamespacedName: client.ObjectKeyFromObject(m)}
}

// ── rebootOwed predicate ──────────────────────────────────────────────────────

func TestRebootOwed(t *testing.T) {
	base := metav1.NewTime(time.Now())
	older := metav1.NewTime(base.Add(-time.Hour))
	newer := metav1.NewTime(base.Add(time.Hour))

	cases := []struct {
		name string
		req  *metav1.Time
		last *metav1.Time
		want bool
	}{
		{"no request", nil, nil, false},
		{"no request but has last reboot", nil, &older, false},
		{"requested, never rebooted", &base, nil, true},
		{"requested newer than last reboot", &base, &older, true},
		{"already rebooted after request", &base, &newer, false},
		{"request equals last reboot is not owed", &base, &base, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := api.MachineSpec{RebootRequestedAt: tc.req}
			status := api.MachineStatus{LastRebootTime: tc.last}
			if got := rebootOwed(spec, status); got != tc.want {
				t.Errorf("rebootOwed = %v, want %v", got, tc.want)
			}
		})
	}
}

// ── omniRoleLabel ─────────────────────────────────────────────────────────────

func TestOmniRoleLabel(t *testing.T) {
	cases := []struct {
		role    string
		want    string
		wantErr bool
	}{
		{roleControlPlane, omnires.LabelControlPlaneRole, false},
		{roleWorker, omnires.LabelWorkerRole, false},
		{"bogus", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			got, err := omniRoleLabel(tc.role)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for role %q", tc.role)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("omniRoleLabel(%q) = %q, want %q", tc.role, got, tc.want)
			}
		})
	}
}

// ── finalizer teardown ordering ───────────────────────────────────────────────

// TestMachineReconciler_TeardownOrdering verifies that deleting a Machine runs
// the finalizer teardown in the correct order — unbind → config patches →
// kernel args — and then clears the finalizer so the object is removed.
func TestMachineReconciler_TeardownOrdering(t *testing.T) {
	ctx := context.Background()

	machine := &api.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "machine-uuid-1",
			Namespace:  "default",
			Finalizers: []string{machineFinalizerName},
		},
		Spec: api.MachineSpec{ClusterName: "test-cluster", MachineSetID: "test-cluster-workers", Role: roleWorker},
	}
	fakeOmni := &fakeMachineOmniClient{}
	r := newMachineReconciler(t, fakeOmni, machine)

	// Delete the Machine: the finalizer keeps it around with a deletion timestamp.
	if err := r.Delete(ctx, machine); err != nil {
		t.Fatalf("delete machine: %v", err)
	}

	if _, err := r.Reconcile(ctx, machineReq(machine)); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}

	want := []string{"unbind", "deleteConfig", "deleteKernel"}
	if !slices.Equal(fakeOmni.calls, want) {
		t.Errorf("teardown call order = %v, want %v", fakeOmni.calls, want)
	}

	// Finalizer cleared → object gone.
	got := &api.Machine{}
	err := r.Get(ctx, client.ObjectKeyFromObject(machine), got)
	if err == nil {
		t.Errorf("machine should be gone after finalizer teardown, still present")
	}
}

// ── bind + config + kernel args ───────────────────────────────────────────────

// TestMachineReconciler_ConvergesOmniState verifies the happy path against a real
// in-memory Omni state: the machine is bound, its config patches and kernel args
// are created, and clearing them on a later reconcile deletes them.
func TestMachineReconciler_ConvergesOmniState(t *testing.T) {
	ctx := context.Background()
	st := newTestState()
	oc := newTestOmniClient(st)

	clusterName := "test-cluster"
	machineSetID := "test-cluster-workers"
	machineID := "machine-uuid-2"

	machine := &api.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:       machineID,
			Namespace:  "default",
			Finalizers: []string{machineFinalizerName},
		},
		Spec: api.MachineSpec{
			ClusterName:  clusterName,
			MachineSetID: machineSetID,
			Role:         roleWorker,
			KernelArgs:   []string{"libata.force=noncq"},
			ConfigPatches: []api.ConfigPatch{
				{Name: "install-disk", Inline: apiextensionsv1.JSON{Raw: mustJSON(map[string]any{"machine": map[string]any{"install": map[string]any{"disk": "/dev/sda"}}})}},
			},
		},
	}
	r := newMachineReconciler(t, oc, machine)

	if _, err := r.Reconcile(ctx, machineReq(machine)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	nodeMD := resource.NewMetadata(omniresources.DefaultNamespace, omnires.MachineSetNodeType, machineID, resource.VersionUndefined)
	if _, err := safe.StateGet[*omnires.MachineSetNode](ctx, st, nodeMD); err != nil {
		t.Fatalf("machine should be bound (MachineSetNode present): %v", err)
	}
	kaMD := resource.NewMetadata(omniresources.DefaultNamespace, omnires.KernelArgsType, machineID, resource.VersionUndefined)
	if _, err := safe.StateGet[*omnires.KernelArgs](ctx, st, kaMD); err != nil {
		t.Fatalf("kernel args should exist: %v", err)
	}
	patchID := fmt.Sprintf("%s-%s-%s", clusterName, machineID, "install-disk")
	patchMD := resource.NewMetadata(omniresources.DefaultNamespace, omnires.ConfigPatchType, patchID, resource.VersionUndefined)
	if _, err := safe.StateGet[*omnires.ConfigPatch](ctx, st, patchMD); err != nil {
		t.Fatalf("config patch should exist: %v", err)
	}

	// Status should reflect a bound, ready machine.
	got := &api.Machine{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(machine), got); err != nil {
		t.Fatalf("get machine: %v", err)
	}
	if !got.Status.Ready || got.Status.Phase != "Ready" {
		t.Errorf("expected Ready/Ready status, got ready=%v phase=%q", got.Status.Ready, got.Status.Phase)
	}

	// Clear kernel args and config patches; a re-reconcile must delete them.
	got.Spec.KernelArgs = nil
	got.Spec.ConfigPatches = nil
	if err := r.Update(ctx, got); err != nil {
		t.Fatalf("update machine: %v", err)
	}
	if _, err := r.Reconcile(ctx, machineReq(machine)); err != nil {
		t.Fatalf("reconcile after clearing: %v", err)
	}

	if _, err := safe.StateGet[*omnires.KernelArgs](ctx, st, kaMD); err == nil {
		t.Error("kernel args should have been deleted when cleared")
	} else if !state.IsNotFoundError(err) {
		t.Fatalf("unexpected error checking kernel args: %v", err)
	}
	if _, err := safe.StateGet[*omnires.ConfigPatch](ctx, st, patchMD); err == nil {
		t.Error("config patch should have been pruned when cleared")
	} else if !state.IsNotFoundError(err) {
		t.Fatalf("unexpected error checking config patch: %v", err)
	}
}

// ── reboot path ───────────────────────────────────────────────────────────────

// TestMachineReconciler_RebootStampsAfterRPC verifies the ordering fix from PR
// #28 at single-machine blast radius: when a reboot is owed, the reboot RPC runs
// (with the requested management address) and Status.LastRebootTime is stamped
// only after it returns, making the reboot no longer owed.
func TestMachineReconciler_RebootStampsAfterRPC(t *testing.T) {
	ctx := context.Background()

	reqAt := metav1.NewTime(time.Now())
	machine := &api.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "machine-uuid-3",
			Namespace:  "default",
			Finalizers: []string{machineFinalizerName},
		},
		Spec: api.MachineSpec{
			ClusterName:       "test-cluster",
			MachineSetID:      "test-cluster-workers",
			Role:              roleWorker,
			RebootRequestedAt: &reqAt,
			ManagementAddress: "10.0.0.7",
		},
	}
	fakeOmni := &fakeMachineOmniClient{}
	r := newMachineReconciler(t, fakeOmni, machine)
	// The success path increments the shared reboot counter; clean the series up
	// so it does not leak into TestClusterMetrics_SetAndDelete's total-count check.
	t.Cleanup(func() { machineRebootsTotal.DeleteLabelValues("test-cluster", "default") })

	if _, err := r.Reconcile(ctx, machineReq(machine)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The reboot RPC ran, targeting the requested management address, after bind.
	bindIdx := slices.Index(fakeOmni.calls, "bind")
	rebootIdx := slices.Index(fakeOmni.calls, "reboot")
	if bindIdx < 0 || rebootIdx < 0 || rebootIdx < bindIdx {
		t.Fatalf("expected bind before reboot, calls=%v", fakeOmni.calls)
	}
	if !slices.Contains(fakeOmni.rebootedAddrs, "10.0.0.7") {
		t.Errorf("expected reboot RPC to target 10.0.0.7, got %v", fakeOmni.rebootedAddrs)
	}

	got := &api.Machine{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(machine), got); err != nil {
		t.Fatalf("get machine: %v", err)
	}
	if got.Status.LastRebootTime == nil {
		t.Fatal("LastRebootTime should be stamped after the reboot RPC")
	}
	// Reboot is no longer owed once stamped — no field-clearing handshake needed.
	if rebootOwed(got.Spec, got.Status) {
		t.Error("reboot should no longer be owed after LastRebootTime is stamped")
	}
}

// TestMachineReconciler_RebootStampsEvenOnRPCError verifies that a failing
// reboot RPC still stamps LastRebootTime (after the RPC), so the machine is not
// hammered with retries every reconcile — it becomes eligible again only after
// the cluster controller requests a fresh reboot past the cooldown.
func TestMachineReconciler_RebootStampsEvenOnRPCError(t *testing.T) {
	ctx := context.Background()

	reqAt := metav1.NewTime(time.Now())
	machine := &api.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "machine-uuid-4",
			Namespace:  "default",
			Finalizers: []string{machineFinalizerName},
		},
		Spec: api.MachineSpec{
			ClusterName:       "test-cluster",
			MachineSetID:      "test-cluster-workers",
			Role:              roleWorker,
			RebootRequestedAt: &reqAt,
			ManagementAddress: "10.0.0.8",
		},
	}
	fakeOmni := &fakeMachineOmniClient{rebootErr: errors.New("rpc boom")}
	r := newMachineReconciler(t, fakeOmni, machine)

	if _, err := r.Reconcile(ctx, machineReq(machine)); err != nil {
		t.Fatalf("reconcile should not return an error on reboot RPC failure: %v", err)
	}

	if !slices.Contains(fakeOmni.calls, "reboot") {
		t.Fatalf("expected reboot RPC to have been attempted, calls=%v", fakeOmni.calls)
	}
	got := &api.Machine{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(machine), got); err != nil {
		t.Fatalf("get machine: %v", err)
	}
	if got.Status.LastRebootTime == nil {
		t.Error("LastRebootTime should be stamped even when the reboot RPC fails")
	}
	if rebootOwed(got.Spec, got.Status) {
		t.Error("reboot should no longer be owed after a stamped attempt")
	}
}

// TestMachineReconciler_NoRebootWhenNotOwed verifies that no reboot RPC is issued
// when Status.LastRebootTime is already at or after Spec.RebootRequestedAt.
func TestMachineReconciler_NoRebootWhenNotOwed(t *testing.T) {
	ctx := context.Background()

	reqAt := metav1.NewTime(time.Now().Add(-time.Hour))
	lastReboot := metav1.NewTime(time.Now())
	machine := &api.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "machine-uuid-5",
			Namespace:  "default",
			Finalizers: []string{machineFinalizerName},
		},
		Spec: api.MachineSpec{
			ClusterName:       "test-cluster",
			MachineSetID:      "test-cluster-workers",
			Role:              roleWorker,
			RebootRequestedAt: &reqAt,
			ManagementAddress: "10.0.0.9",
		},
		Status: api.MachineStatus{LastRebootTime: &lastReboot},
	}
	fakeOmni := &fakeMachineOmniClient{}
	r := newMachineReconciler(t, fakeOmni, machine)

	if _, err := r.Reconcile(ctx, machineReq(machine)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if slices.Contains(fakeOmni.calls, "reboot") {
		t.Errorf("no reboot should be issued when not owed, calls=%v", fakeOmni.calls)
	}
}

// TestMachineReconciler_AddsFinalizer verifies that a Machine with no finalizer
// gets one on first reconcile (and requeues), before any Omni work.
func TestMachineReconciler_AddsFinalizer(t *testing.T) {
	ctx := context.Background()
	machine := &api.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-uuid-6", Namespace: "default"},
		Spec:       api.MachineSpec{ClusterName: "test-cluster", MachineSetID: "test-cluster-workers", Role: roleWorker},
	}
	fakeOmni := &fakeMachineOmniClient{}
	r := newMachineReconciler(t, fakeOmni, machine)

	if _, err := r.Reconcile(ctx, machineReq(machine)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &api.Machine{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(machine), got); err != nil {
		t.Fatalf("get machine: %v", err)
	}
	if !controllerutil.ContainsFinalizer(got, machineFinalizerName) {
		t.Error("finalizer should have been added")
	}
	if len(fakeOmni.calls) != 0 {
		t.Errorf("no Omni work should happen before the finalizer is set, calls=%v", fakeOmni.calls)
	}
}
