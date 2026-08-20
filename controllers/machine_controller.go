package controllers

import (
	"context"
	"encoding/json"
	"fmt"

	omnires "github.com/siderolabs/omni/client/pkg/omni/resources/omni"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	api "github.com/cgoolsby/omni-gitops-controller/api/v1alpha1"
)

const (
	// machineFinalizerName guards a Machine so its Omni-side state (bind,
	// config patches, kernel args) is unwound before the object is removed.
	machineFinalizerName = "machine.omni.gitops.dev/finalizer"

	// Labels stamped on every Machine so the cluster controller can List the
	// machines allocated to a cluster / set instead of keeping a bookkeeping map.
	labelCluster    = "omni.gitops.dev/cluster"
	labelMachineSet = "omni.gitops.dev/machine-set"

	// Machine roles, as stored in Machine.Spec.Role.
	roleControlPlane = "control-plane"
	roleWorker       = "worker"
)

// machineOmniClient is the subset of *OmniClient the MachineReconciler needs.
// Extracting it as an interface gives the reconciler an injection seam so the
// per-machine bind/config/reboot/teardown paths are unit-testable without a
// live Omni server (the testability gap noted during the #27 review).
type machineOmniClient interface {
	EnsureMachineSetNode(ctx context.Context, clusterName, machineSetID, machineID, role string) error
	DeleteMachineSetNode(ctx context.Context, machineID string) error
	EnsureConfigPatch(ctx context.Context, patchID, clusterName, machineSetID, machineID string, inline json.RawMessage) error
	PruneOrphanedConfigPatches(ctx context.Context, clusterName, machineID string, keepIDs map[string]struct{}) error
	DeleteConfigPatchesForMachine(ctx context.Context, clusterName, machineID string) error
	EnsureMachineKernelArgs(ctx context.Context, machineID string, args []string) error
	DeleteKernelArgsForMachine(ctx context.Context, machineID string) error
	RebootMachine(ctx context.Context, clusterName, managementAddress string) error
}

// Compile-time assertion that the concrete client satisfies the seam.
var _ machineOmniClient = (*OmniClient)(nil)

// MachineReconciler owns all per-machine Omni state for a single Machine:
// bind/unbind to its Omni machine set, its config patches and kernel args, and
// its drift-remediation reboots. Shrinking per-machine responsibilities to this
// controller keeps a per-machine bug from blasting the whole cluster reconcile.
type MachineReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	OmniClient machineOmniClient
	Recorder   events.EventRecorder
}

// eventf records an event on the machine. Recorder may be nil (unit tests
// construct the reconciler without a manager), in which case this is a no-op.
func (r *MachineReconciler) eventf(machine *api.Machine, eventType, reason, format string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(machine, nil, eventType, reason, reason, format, args...)
	}
}

// +kubebuilder:rbac:groups=omni.gitops.dev,resources=machines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=omni.gitops.dev,resources=machines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=omni.gitops.dev,resources=machines/finalizers,verbs=update

func (r *MachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	machine := &api.Machine{}
	if err := r.Get(ctx, req.NamespacedName, machine); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// ── Delete path ───────────────────────────────────────────────────────────
	if !machine.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(machine, machineFinalizerName) {
			if err := r.teardown(ctx, machine); err != nil {
				return ctrl.Result{}, fmt.Errorf("teardown machine %s: %w", machine.Name, err)
			}
			controllerutil.RemoveFinalizer(machine, machineFinalizerName)
			if err := r.Update(ctx, machine); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// ── Add finalizer ─────────────────────────────────────────────────────────
	if !controllerutil.ContainsFinalizer(machine, machineFinalizerName) {
		controllerutil.AddFinalizer(machine, machineFinalizerName)
		if err := r.Update(ctx, machine); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	statusBefore := machine.Status.DeepCopy()

	role, err := omniRoleLabel(machine.Spec.Role)
	if err != nil {
		return r.setFailure(ctx, machine, statusBefore, "InvalidRole", err)
	}

	// Bind the machine to its set in Omni. Idempotent; errors loudly if the
	// machine was bound to a different set out-of-band.
	if err := r.OmniClient.EnsureMachineSetNode(ctx, machine.Spec.ClusterName, machine.Spec.MachineSetID, machine.Name, role); err != nil {
		return r.setFailure(ctx, machine, statusBefore, "BindFailed", err)
	}

	// Apply this machine's config patches, pruning any no longer desired.
	if err := r.applyConfigPatches(ctx, machine); err != nil {
		return r.setFailure(ctx, machine, statusBefore, "ConfigPatchFailed", err)
	}

	// Reconcile kernel args (schematic-level). Delete the resource when cleared.
	if len(machine.Spec.KernelArgs) > 0 {
		if err := r.OmniClient.EnsureMachineKernelArgs(ctx, machine.Name, machine.Spec.KernelArgs); err != nil {
			return r.setFailure(ctx, machine, statusBefore, "KernelArgsFailed", err)
		}
	} else {
		if err := r.OmniClient.DeleteKernelArgsForMachine(ctx, machine.Name); err != nil {
			return r.setFailure(ctx, machine, statusBefore, "KernelArgsFailed", err)
		}
	}

	// ── Reboot when owed ──────────────────────────────────────────────────────
	// A reboot is owed iff Spec.RebootRequestedAt is newer than
	// Status.LastRebootTime. Status.LastRebootTime is stamped only after the RPC
	// returns — the ordering fix from PR #28 at single-machine blast radius — so
	// the stamp is never persisted without a matching attempt, and stamping it
	// makes the reboot naturally no longer owed with no field-clearing handshake.
	rebootFailed := false
	if rebootOwed(machine.Spec, machine.Status) {
		rebootErr := r.OmniClient.RebootMachine(ctx, machine.Spec.ClusterName, machine.Spec.ManagementAddress)
		now := metav1.Now()
		machine.Status.LastRebootTime = &now
		if rebootErr != nil {
			logger.Error(rebootErr, "Failed to reboot machine", "machine", machine.Name)
			r.eventf(machine, corev1.EventTypeWarning, "RebootFailed", "%s", rebootErr.Error())
			rebootFailed = true
		} else {
			machineRebootsTotal.WithLabelValues(machine.Spec.ClusterName, machine.Namespace).Inc()
			r.eventf(machine, corev1.EventTypeNormal, "RebootTriggered",
				"Rebooting machine %s to apply config update", machine.Name)
		}
	}

	// ── Status ────────────────────────────────────────────────────────────────
	machine.Status.ObservedGeneration = machine.Generation
	machine.Status.FailureReason = nil
	machine.Status.FailureMessage = nil
	if rebootFailed {
		machine.Status.Ready = false
		machine.Status.Phase = "Rebooting"
		meta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "RebootFailed",
			Message:            "Reboot RPC failed; will retry after cooldown",
			ObservedGeneration: machine.Generation,
		})
	} else {
		machine.Status.Ready = true
		machine.Status.Phase = "Ready"
		meta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Bound",
			Message:            "Machine is bound and its config is applied",
			ObservedGeneration: machine.Generation,
		})
	}

	if machineStatusNeedsUpdate(statusBefore, &machine.Status) {
		if err := r.Status().Update(ctx, machine); err != nil {
			return ctrl.Result{}, err
		}
	}

	if rebootFailed {
		return ctrl.Result{RequeueAfter: requeueShort}, nil
	}
	logger.Info("Machine reconciled", "machine", machine.Name, "set", machine.Spec.MachineSetID)
	return ctrl.Result{}, nil
}

// teardown unwinds a machine's Omni-side state: unbind → config patches →
// kernel args. This is the former OmniClusterReconciler.releaseMachine body,
// now a Machine finalizer. DeleteMachineSetNode returns a retryable error while
// Omni teardown is in progress, which makes controller-runtime requeue.
func (r *MachineReconciler) teardown(ctx context.Context, machine *api.Machine) error {
	return releaseMachineOmni(ctx, r.OmniClient, machine.Spec.ClusterName, machine.Name)
}

// applyConfigPatches creates or updates Omni ConfigPatches for this machine,
// then deletes any patches that are no longer in the desired set.
func (r *MachineReconciler) applyConfigPatches(ctx context.Context, machine *api.Machine) error {
	keepIDs := make(map[string]struct{}, len(machine.Spec.ConfigPatches))
	for _, p := range machine.Spec.ConfigPatches {
		patchID := fmt.Sprintf("%s-%s-%s", machine.Spec.ClusterName, machine.Name, p.Name)
		keepIDs[patchID] = struct{}{}
		if err := r.OmniClient.EnsureConfigPatch(ctx, patchID, machine.Spec.ClusterName, machine.Spec.MachineSetID, machine.Name, json.RawMessage(p.Inline.Raw)); err != nil {
			return fmt.Errorf("ensure patch %q: %w", p.Name, err)
		}
	}
	if err := r.OmniClient.PruneOrphanedConfigPatches(ctx, machine.Spec.ClusterName, machine.Name, keepIDs); err != nil {
		return fmt.Errorf("prune patches for machine %s: %w", machine.Name, err)
	}
	return nil
}

// setFailure records a per-machine failure in status and requeues. Like the
// cluster controller's setFailure it returns a nil error so RequeueAfter is
// honoured instead of controller-runtime's exponential backoff.
func (r *MachineReconciler) setFailure(ctx context.Context, machine *api.Machine, statusBefore *api.MachineStatus, reason string, err error) (ctrl.Result, error) {
	msg := err.Error()
	r.eventf(machine, corev1.EventTypeWarning, reason, "%s", msg)
	machine.Status.ObservedGeneration = machine.Generation
	machine.Status.Ready = false
	machine.Status.Phase = "Failed"
	machine.Status.FailureReason = &reason
	machine.Status.FailureMessage = &msg
	meta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: machine.Generation,
	})
	if machineStatusNeedsUpdate(statusBefore, &machine.Status) {
		_ = r.Status().Update(ctx, machine)
	}
	return ctrl.Result{RequeueAfter: requeueShort}, nil
}

func (r *MachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&api.Machine{}).
		Complete(r)
}

// ── helpers ─────────────────────────────────────────────────────────────────

// releaseMachineOmni unbinds a machine from its set and removes its ConfigPatches
// and KernelArgs, so it returns to the available pool without carrying stale
// customisation into the next cluster that allocates it. Shared by the Machine
// finalizer teardown and the cluster controller's Omni-state backstop.
func releaseMachineOmni(ctx context.Context, oc machineOmniClient, clusterName, machineID string) error {
	if err := oc.DeleteMachineSetNode(ctx, machineID); err != nil {
		return fmt.Errorf("delete machine set node %s: %w", machineID, err)
	}
	if err := oc.DeleteConfigPatchesForMachine(ctx, clusterName, machineID); err != nil {
		return fmt.Errorf("delete config patches for machine %s: %w", machineID, err)
	}
	if err := oc.DeleteKernelArgsForMachine(ctx, machineID); err != nil {
		return fmt.Errorf("delete kernel args for machine %s: %w", machineID, err)
	}
	return nil
}

// rebootOwed reports whether a drift-remediation reboot is owed for a machine:
// Spec.RebootRequestedAt is set and newer than Status.LastRebootTime. Stamping
// LastRebootTime (only after the reboot RPC runs) naturally makes it no longer
// owed — there is no request field to clear.
func rebootOwed(spec api.MachineSpec, status api.MachineStatus) bool {
	if spec.RebootRequestedAt == nil {
		return false
	}
	if status.LastRebootTime == nil {
		return true
	}
	return spec.RebootRequestedAt.After(status.LastRebootTime.Time)
}

// omniRoleLabel maps a Machine.Spec.Role to the Omni role label key used when
// binding the machine to its set.
func omniRoleLabel(role string) (string, error) {
	switch role {
	case roleControlPlane:
		return omnires.LabelControlPlaneRole, nil
	case roleWorker:
		return omnires.LabelWorkerRole, nil
	default:
		return "", fmt.Errorf("unknown machine role %q", role)
	}
}

// machineStatusNeedsUpdate reports whether the machine status changed and must
// be written back. Skipping no-op writes avoids retriggering reconcile through
// the controller's own watch.
func machineStatusNeedsUpdate(before, after *api.MachineStatus) bool {
	return !equality.Semantic.DeepEqual(before, after)
}
