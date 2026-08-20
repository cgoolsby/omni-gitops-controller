package controllers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	goerrors "errors"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	omnires "github.com/siderolabs/omni/client/pkg/omni/resources/omni"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	api "github.com/cgoolsby/omni-gitops-controller/api/v1alpha1"
)

const (
	finalizerName = "omnicluster.omni.gitops.dev/finalizer"
	requeueShort  = 15 * time.Second
	requeueLong   = 60 * time.Second
	// rebootCooldown is the minimum time between drift-remediation reboots of the
	// same machine, so a machine whose drift persists across reboots is not
	// rebooted in a loop every reconcile cycle.
	rebootCooldown = 10 * time.Minute

	kubeconfigRefreshedAtAnnotation = "omni.gitops.dev/kubeconfig-refreshed-at"
	// kubeconfigRefreshInterval is how often the kubeconfig Secret is re-fetched
	// from Omni. Must be comfortably shorter than the 90-day token TTL requested
	// in GetKubeconfig so the token never expires in place.
	kubeconfigRefreshInterval = 30 * 24 * time.Hour
)

// OmniClusterReconciler reconciles OmniCluster objects.
type OmniClusterReconciler struct {
	client.Client
	Scheme              *runtime.Scheme
	OmniClient          *OmniClient
	Recorder            events.EventRecorder
	KubeconfigNamespace string
	ArgoCDClusters      bool
}

// eventf records an event on the cluster. Recorder may be nil (unit tests
// construct the reconciler without a manager), in which case this is a no-op.
func (r *OmniClusterReconciler) eventf(cluster *api.OmniCluster, eventType, reason, format string, args ...any) {
	if r.Recorder != nil {
		// The new events API adds a related object and an action; we have no
		// secondary object, and the reason doubles as the action.
		r.Recorder.Eventf(cluster, nil, eventType, reason, reason, format, args...)
	}
}

// +kubebuilder:rbac:groups=omni.gitops.dev,resources=omniclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=omni.gitops.dev,resources=omniclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=omni.gitops.dev,resources=omniclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update;patch;delete

func (r *OmniClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	cluster := &api.OmniCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// ── Delete path ───────────────────────────────────────────────────────────
	if !cluster.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(cluster, finalizerName) {
			if err := r.deleteOmniResources(ctx, cluster); err != nil {
				return ctrl.Result{}, fmt.Errorf("delete omni resources: %w", err)
			}
			deleteClusterMetrics(cluster.Name, cluster.Namespace)
			controllerutil.RemoveFinalizer(cluster, finalizerName)
			if err := r.Update(ctx, cluster); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// ── Add finalizer ─────────────────────────────────────────────────────────
	if !controllerutil.ContainsFinalizer(cluster, finalizerName) {
		controllerutil.AddFinalizer(cluster, finalizerName)
		if err := r.Update(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Snapshot status so the write at the end can be skipped when nothing
	// changed: an unconditional status update fires a watch event on our own
	// CR, which would retrigger reconcile in a tight loop.
	statusBefore := cluster.Status.DeepCopy()

	// ── Reconcile Omni resources ──────────────────────────────────────────────
	clusterName := cluster.Name

	// These helpers record their own failure via setFailure (which returns a nil
	// error so its RequeueAfter is honored), so they signal "stop and return"
	// through an explicit bool rather than the error — err is nil on both the
	// success and failure paths.
	if result, stop := r.ensureCluster(ctx, cluster, statusBefore, clusterName); stop {
		return result, nil
	}

	allocatedMachines := map[string][]string{}
	if result, stop := r.allocateControlPlaneMachines(ctx, cluster, statusBefore, clusterName, allocatedMachines); stop {
		return result, nil
	}
	if result, stop := r.allocateWorkerMachines(ctx, cluster, statusBefore, clusterName, allocatedMachines); stop {
		return result, nil
	}

	// Prune machine sets that exist in Omni but are no longer declared in spec.
	// allocatedMachines is built only from spec-declared sets above, so pruned
	// sets never appear in the status update below.
	if err := r.pruneOrphanedMachineSets(ctx, cluster, clusterName); err != nil {
		return r.setFailure(ctx, cluster, statusBefore, "PruneOrphanedMachineSetFailed", err)
	}

	// ── Update status from Omni ───────────────────────────────────────────────
	omniStatus, err := r.OmniClient.GetClusterStatus(ctx, clusterName)
	if err != nil {
		return r.setFailure(ctx, cluster, statusBefore, "GetStatusFailed", err)
	}

	// Record status from Omni before the drift check below, which can itself
	// fail and return early: that must not strand this already-fetched Phase
	// and AllocatedMachines behind a stale value from a prior reconcile.
	cluster.Status.ObservedGeneration = cluster.Generation
	cluster.Status.Phase = omniStatus.Phase
	cluster.Status.Ready = omniStatus.Ready
	cluster.Status.AllocatedMachines = allocatedMachines
	cluster.Status.FailureReason = nil
	cluster.Status.FailureMessage = nil
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             boolToConditionStatus(omniStatus.Ready),
		Reason:             omniStatus.Phase,
		Message:            fmt.Sprintf("Omni cluster phase: %s", omniStatus.Phase),
		ObservedGeneration: cluster.Generation,
	})

	clusterReadyGauge.WithLabelValues(cluster.Name, cluster.Namespace).Set(boolToFloat64(omniStatus.Ready))

	// Reset the per-machineset series before re-setting them so machine sets
	// removed from spec do not linger as stale series.
	clusterMachinesAllocatedGauge.DeletePartialMatch(prometheus.Labels{
		"name":      cluster.Name,
		"namespace": cluster.Namespace,
	})
	for machineSetID, machines := range allocatedMachines {
		clusterMachinesAllocatedGauge.WithLabelValues(cluster.Name, cluster.Namespace, machineSetID).Set(float64(len(machines)))
	}

	// Detect config drift only when the cluster is ready (machines must be Running).
	var drifting []MachineConfigDrift
	if omniStatus.Ready {
		drifting, err = r.OmniClient.GetDriftingMachines(ctx, clusterName)
		if err != nil {
			return r.setFailure(ctx, cluster, statusBefore, "ConfigDriftCheckFailed", err)
		}
	}
	clusterConfigDriftGauge.WithLabelValues(cluster.Name, cluster.Namespace).Set(boolToFloat64(len(drifting) > 0))

	// Prune reboot bookkeeping for machines no longer allocated to this cluster.
	allocatedSet := map[string]bool{}
	for _, ids := range allocatedMachines {
		for _, id := range ids {
			allocatedSet[id] = true
		}
	}
	for id := range cluster.Status.LastRebootTimes {
		if !allocatedSet[id] {
			delete(cluster.Status.LastRebootTimes, id)
		}
	}

	now := metav1.Now()
	var rebootCandidate *MachineConfigDrift
	if len(drifting) == 0 {
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               "ConfigDriftDetected",
			Status:             metav1.ConditionFalse,
			Reason:             "AllMachinesUpToDate",
			Message:            "All machines are running the target config",
			ObservedGeneration: cluster.Generation,
		})
	} else if rebootCandidate = selectRebootCandidate(drifting, cluster.Status.LastRebootTimes, now.Time); rebootCandidate != nil {
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               "ConfigDriftDetected",
			Status:             metav1.ConditionTrue,
			Reason:             "PendingReboot",
			Message:            fmt.Sprintf("Machine %s has pending config update; reboot scheduled", rebootCandidate.MachineID),
			ObservedGeneration: cluster.Generation,
		})
	} else {
		waiting := make([]string, 0, len(drifting))
		for _, d := range drifting {
			waiting = append(waiting, d.MachineID)
		}
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               "ConfigDriftDetected",
			Status:             metav1.ConditionTrue,
			Reason:             "RebootCooldown",
			Message:            fmt.Sprintf("Machine(s) %s still drifting after a recent reboot; next reboot allowed after a %s cooldown", strings.Join(waiting, ", "), rebootCooldown),
			ObservedGeneration: cluster.Generation,
		})
		r.eventf(cluster, corev1.EventTypeWarning, "RebootCooldown",
			"Machine(s) %s still drifting after a recent reboot; waiting for the %s cooldown", strings.Join(waiting, ", "), rebootCooldown)
	}

	// Every Omni-side action this reconcile needs to perform (kubeconfig, reboot)
	// runs here, before the single status write below, so that write always
	// reflects this pass's fully settled outcome instead of a still-pending
	// intent, and no later pass needs to discover an action was never taken.
	rebootFailed := false
	if omniStatus.Ready {
		var (
			kubeconfigName    string
			kubeconfigWritten bool
			kubeconfigErr     error
		)
		if r.ArgoCDClusters {
			kubeconfigName = clusterName + "-cluster-secret"
			kubeconfigWritten, kubeconfigErr = r.ensureArgoCDClusterSecret(ctx, clusterName)
		} else {
			kubeconfigName = clusterName + "-kubeconfig"
			kubeconfigWritten, kubeconfigErr = r.ensureKubeconfigSecret(ctx, clusterName)
		}
		if kubeconfigErr != nil {
			return r.setFailure(ctx, cluster, statusBefore, "EnsureKubeconfigFailed", kubeconfigErr)
		}
		if kubeconfigWritten {
			r.eventf(cluster, corev1.EventTypeNormal, "KubeconfigWritten",
				"Kubeconfig written to %s/%s", r.KubeconfigNamespace, kubeconfigName)
		}

		// Reboot the selected drifting machine (rolling window = 1). The window is enforced by
		// GetDriftingMachines, which returns no candidates while any machine in the cluster is
		// unhealthy — so a machine still rebooting from the previous cycle blocks further reboots.
		if rebootCandidate != nil {
			rebootFailed = r.rebootMachine(ctx, cluster, clusterName, rebootCandidate)
			// Record the attempt time after issuing the reboot, regardless of
			// outcome, so a failed RPC is not retried against the same machine
			// every cycle. Stamping only here, right after the RPC actually ran,
			// guarantees this is never persisted without a matching attempt.
			if cluster.Status.LastRebootTimes == nil {
				cluster.Status.LastRebootTimes = map[string]metav1.Time{}
			}
			cluster.Status.LastRebootTimes[rebootCandidate.MachineID] = now
		}
	}

	if statusNeedsUpdate(statusBefore, &cluster.Status) {
		if err := r.Status().Update(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}
	}

	if !omniStatus.Ready {
		logger.Info("Cluster not yet ready, requeuing", "phase", omniStatus.Phase)
		return ctrl.Result{RequeueAfter: requeueShort}, nil
	}
	if rebootFailed {
		return ctrl.Result{RequeueAfter: requeueShort}, nil
	}
	if rebootCandidate != nil {
		return ctrl.Result{RequeueAfter: requeueLong}, nil
	}

	logger.Info("Cluster reconciled", "phase", omniStatus.Phase)
	return ctrl.Result{RequeueAfter: requeueLong}, nil
}

// rebootMachine issues the reboot RPC for the selected drifting machine,
// logging and emitting an Event for either outcome. It reports whether the
// RPC failed; the caller is responsible for recording the attempt in status
// regardless of the outcome.
func (r *OmniClusterReconciler) rebootMachine(ctx context.Context, cluster *api.OmniCluster, clusterName string, candidate *MachineConfigDrift) (failed bool) {
	logger := log.FromContext(ctx)
	logger.Info("Config drift detected, triggering reboot", "machine", candidate.MachineID, "addr", candidate.ManagementAddress)
	if err := r.OmniClient.RebootMachine(ctx, clusterName, candidate.ManagementAddress); err != nil {
		logger.Error(err, "Failed to reboot machine", "machine", candidate.MachineID)
		r.eventf(cluster, corev1.EventTypeWarning, "RebootFailed", "%s", err.Error())
		return true
	}

	machineRebootsTotal.WithLabelValues(cluster.Name, cluster.Namespace).Inc()
	r.eventf(cluster, corev1.EventTypeNormal, "RebootTriggered",
		"Rebooting machine %s to apply config update", candidate.MachineID)
	return false
}

// ensureCluster makes sure the cluster exists in Omni. On failure it records
// the status failure itself and returns (result, true) so the caller returns
// that result directly; on success it returns (_, false) to continue.
func (r *OmniClusterReconciler) ensureCluster(
	ctx context.Context,
	cluster *api.OmniCluster,
	statusBefore *api.OmniClusterStatus,
	clusterName string,
) (ctrl.Result, bool) {
	if err := r.OmniClient.EnsureCluster(ctx,
		clusterName,
		ownerOf(cluster),
		cluster.Spec.KubernetesVersion,
		cluster.Spec.TalosVersion,
	); err != nil {
		reason := "EnsureClusterFailed"
		if goerrors.Is(err, ErrClusterOwnershipConflict) {
			reason = "ClusterOwnershipConflict"
		}
		result, _ := r.setFailure(ctx, cluster, statusBefore, reason, err)
		return result, true
	}
	return ctrl.Result{}, false
}

// allocateControlPlaneMachines ensures the control-plane machine set exists and
// allocates machines to it, recording the result in allocatedMachines. On
// failure it records the status failure itself and returns (result, true) so the
// caller returns that result directly; on success it returns (_, false).
func (r *OmniClusterReconciler) allocateControlPlaneMachines(
	ctx context.Context,
	cluster *api.OmniCluster,
	statusBefore *api.OmniClusterStatus,
	clusterName string,
	allocatedMachines map[string][]string,
) (ctrl.Result, bool) {
	cpMachineSetID := omnires.ControlPlanesResourceID(clusterName)
	if err := r.OmniClient.EnsureMachineSet(ctx, clusterName, cpMachineSetID, omnires.LabelControlPlaneRole); err != nil {
		result, _ := r.setFailure(ctx, cluster, statusBefore, "EnsureMachineSetFailed", err)
		return result, true
	}

	allocated, err := r.reconcileMachineSet(ctx, cluster, clusterName, cpMachineSetID,
		omnires.LabelControlPlaneRole, cluster.Spec.ControlPlane)
	if err != nil {
		result, _ := r.setFailure(ctx, cluster, statusBefore, "AllocateCPMachinesFailed", err)
		return result, true
	}
	allocatedMachines[cpMachineSetID] = allocated
	return ctrl.Result{}, false
}

// allocateWorkerMachines ensures each declared worker machine set exists and
// allocates machines to it, recording each result in allocatedMachines. On
// failure it records the status failure itself and returns (result, true) so the
// caller returns that result directly; on success it returns (_, false).
func (r *OmniClusterReconciler) allocateWorkerMachines(
	ctx context.Context,
	cluster *api.OmniCluster,
	statusBefore *api.OmniClusterStatus,
	clusterName string,
	allocatedMachines map[string][]string,
) (ctrl.Result, bool) {
	for _, w := range cluster.Spec.Workers {
		wID := fmt.Sprintf("%s-%s", clusterName, w.Name)
		if err := r.OmniClient.EnsureMachineSet(ctx, clusterName, wID, omnires.LabelWorkerRole); err != nil {
			result, _ := r.setFailure(ctx, cluster, statusBefore, "EnsureWorkerMachineSetFailed", err)
			return result, true
		}
		allocated, err := r.reconcileMachineSet(ctx, cluster, clusterName, wID,
			omnires.LabelWorkerRole, w.MachineSetSpec)
		if err != nil {
			result, _ := r.setFailure(ctx, cluster, statusBefore, "AllocateWorkerMachinesFailed", err)
			return result, true
		}
		allocatedMachines[wID] = allocated
	}
	return ctrl.Result{}, false
}

// reconcileMachineSet ensures the correct set of MachineSetNodes exist for one machine set.
// It computes needed = desired - already_bound, selects that many available machines, and
// creates MachineSetNodes + ConfigPatches for each new allocation.
// Returns the full list of allocated machine UUIDs for this set.
func (r *OmniClusterReconciler) reconcileMachineSet(
	ctx context.Context,
	cluster *api.OmniCluster,
	clusterName, machineSetID, role string,
	spec api.MachineSetSpec,
) ([]string, error) {
	already, err := r.OmniClient.AllocatedMachineSetNodes(ctx, machineSetID)
	if err != nil {
		return nil, fmt.Errorf("list allocated nodes for %s: %w", machineSetID, err)
	}

	needed := int(spec.Replicas) - len(already)

	if needed > 0 {
		// ── Scale up ──────────────────────────────────────────────────────────
		bound := make(map[string]bool, len(already))
		for _, id := range already {
			bound[id] = true
		}

		candidates, err := r.OmniClient.SelectAvailableMachines(ctx, spec.MachineSelector, needed+len(already))
		if err != nil {
			return nil, fmt.Errorf("select machines for %s: %w", machineSetID, err)
		}

		var fresh []string
		for _, id := range candidates {
			if !bound[id] {
				fresh = append(fresh, id)
				if len(fresh) >= needed {
					break
				}
			}
		}

		if len(fresh) < needed {
			return nil, fmt.Errorf("not enough available machines for %s: need %d more, found %d candidates",
				machineSetID, needed, len(fresh))
		}

		for _, machineID := range fresh {
			if err := r.OmniClient.EnsureMachineSetNode(ctx, clusterName, machineSetID, machineID, role); err != nil {
				return nil, fmt.Errorf("bind machine %s to %s: %w", machineID, machineSetID, err)
			}
			already = append(already, machineID)
		}
		r.eventf(cluster, corev1.EventTypeNormal, "MachinesAllocated",
			"Allocated %d machine(s) to %s", len(fresh), machineSetID)

	} else if needed < 0 {
		// ── Scale down ────────────────────────────────────────────────────────
		toRemove := -needed

		if role == omnires.LabelControlPlaneRole {
			if int(spec.Replicas) < 1 {
				return nil, fmt.Errorf("control plane cannot scale below 1 replica")
			}
			if int(spec.Replicas)%2 == 0 {
				log.FromContext(ctx).Info("WARNING: even number of control-plane replicas loses etcd quorum margin",
					"replicas", spec.Replicas)
			}
			// Remove at most one control-plane member per reconcile. Evicting
			// several etcd members at once (e.g. scaling 5 → 3) risks quorum
			// loss while Omni is still tearing down the first member. The
			// periodic RequeueAfter picks up the next removal on a later
			// cycle, and DeleteMachineSetNode errors while teardown is in
			// progress, so each member is fully removed before the next one.
			if toRemove > 1 {
				toRemove = 1
			}
		}

		evict := already[len(already)-toRemove:]
		keep := already[:len(already)-toRemove]

		for _, machineID := range evict {
			if err := r.releaseMachine(ctx, clusterName, machineID); err != nil {
				return nil, fmt.Errorf("evict machine %s from %s: %w", machineID, machineSetID, err)
			}
		}
		already = keep
	}

	// Apply patches to all machines on every reconcile so updates propagate to existing clusters.
	for _, machineID := range already {
		if err := r.applyConfigPatches(ctx, clusterName, machineSetID, machineID, spec.ConfigPatches); err != nil {
			return nil, fmt.Errorf("config patches for %s: %w", machineID, err)
		}
	}

	// Reconcile extensions configuration at machine-set level.
	// Delete the resource when the extensions list is cleared so Omni revokes the schematic.
	if len(spec.MachineExtensions) > 0 {
		if err := r.OmniClient.EnsureMachineSetExtensionsConfiguration(ctx, clusterName, machineSetID, spec.MachineExtensions); err != nil {
			return nil, fmt.Errorf("extensions configuration for %s: %w", machineSetID, err)
		}
	} else {
		if err := r.OmniClient.DeleteExtensionsConfigurationForMachineSet(ctx, machineSetID); err != nil {
			return nil, fmt.Errorf("delete extensions configuration for %s: %w", machineSetID, err)
		}
	}

	// Reconcile kernel args per-machine (schematic-level, active during maintenance/install boot).
	// Delete the resource when the kernelArgs list is cleared so the args are revoked.
	for _, machineID := range already {
		if len(spec.KernelArgs) > 0 {
			if err := r.OmniClient.EnsureMachineKernelArgs(ctx, machineID, spec.KernelArgs); err != nil {
				return nil, fmt.Errorf("kernel args for machine %s: %w", machineID, err)
			}
		} else {
			if err := r.OmniClient.DeleteKernelArgsForMachine(ctx, machineID); err != nil {
				return nil, fmt.Errorf("delete kernel args for machine %s: %w", machineID, err)
			}
		}
	}

	return already, nil
}

// releaseMachine unbinds a machine from its machine set and removes its
// ConfigPatches and KernelArgs, so the machine returns to the available pool
// without carrying stale customisation into the next cluster that allocates
// it. DeleteMachineSetNode returns an error while Omni teardown is in
// progress, which makes the caller requeue and retry.
func (r *OmniClusterReconciler) releaseMachine(ctx context.Context, clusterName, machineID string) error {
	if err := r.OmniClient.DeleteMachineSetNode(ctx, machineID); err != nil {
		return fmt.Errorf("delete machine set node %s: %w", machineID, err)
	}
	if err := r.OmniClient.DeleteConfigPatchesForMachine(ctx, clusterName, machineID); err != nil {
		return fmt.Errorf("delete config patches for machine %s: %w", machineID, err)
	}
	if err := r.OmniClient.DeleteKernelArgsForMachine(ctx, machineID); err != nil {
		return fmt.Errorf("delete kernel args for machine %s: %w", machineID, err)
	}
	return nil
}

// pruneOrphanedMachineSets deletes Omni MachineSets labelled with this cluster
// that are no longer declared in spec — e.g. when a user removes a worker set
// from spec.workers without deleting the whole OmniCluster. Like
// deleteOmniResources, discovery is driven by Omni state (the authoritative
// source) rather than status.AllocatedMachines, which can be empty or stale.
func (r *OmniClusterReconciler) pruneOrphanedMachineSets(ctx context.Context, cluster *api.OmniCluster, clusterName string) error {
	cpMachineSetID := omnires.ControlPlanesResourceID(clusterName)
	desired := map[string]bool{cpMachineSetID: true}
	for _, w := range cluster.Spec.Workers {
		desired[fmt.Sprintf("%s-%s", clusterName, w.Name)] = true
	}

	actual, err := r.OmniClient.ListMachineSetIDsForCluster(ctx, clusterName)
	if err != nil {
		return fmt.Errorf("list machine sets for cluster %s: %w", clusterName, err)
	}

	for _, machineSetID := range actual {
		if desired[machineSetID] {
			continue
		}
		// The control-plane set is always in desired; guard anyway so a bug in
		// desired-set construction can never tear down the control plane.
		if machineSetID == cpMachineSetID {
			continue
		}

		machines, err := r.OmniClient.AllocatedMachineSetNodes(ctx, machineSetID)
		if err != nil {
			return fmt.Errorf("list machine set nodes for %s: %w", machineSetID, err)
		}
		for _, machineID := range machines {
			if err := r.releaseMachine(ctx, clusterName, machineID); err != nil {
				return fmt.Errorf("release machine from orphaned set %s: %w", machineSetID, err)
			}
		}
		if err := r.OmniClient.DeleteExtensionsConfigurationForMachineSet(ctx, machineSetID); err != nil {
			return fmt.Errorf("delete extensions configuration for orphaned machine set %s: %w", machineSetID, err)
		}
		if err := r.OmniClient.DeleteMachineSet(ctx, machineSetID); err != nil {
			return fmt.Errorf("delete orphaned machine set %s: %w", machineSetID, err)
		}
	}
	return nil
}

// applyConfigPatches creates or updates Omni ConfigPatches for a machine,
// then deletes any patches that are no longer in the desired set.
func (r *OmniClusterReconciler) applyConfigPatches(
	ctx context.Context,
	clusterName, machineSetID, machineID string,
	patches []api.ConfigPatch,
) error {
	keepIDs := make(map[string]struct{}, len(patches))
	for _, p := range patches {
		patchID := fmt.Sprintf("%s-%s-%s", clusterName, machineID, p.Name)
		keepIDs[patchID] = struct{}{}
		if err := r.OmniClient.EnsureConfigPatch(ctx, patchID, clusterName, machineSetID, machineID, json.RawMessage(p.Inline.Raw)); err != nil {
			return fmt.Errorf("ensure patch %q: %w", p.Name, err)
		}
	}
	if err := r.OmniClient.PruneOrphanedConfigPatches(ctx, clusterName, machineID, keepIDs); err != nil {
		return fmt.Errorf("prune patches for machine %s: %w", machineID, err)
	}
	return nil
}

// deleteOmniResources tears down all Omni resources for this cluster in reverse
// order. Teardown is driven entirely by Omni state (the authoritative source)
// rather than status.AllocatedMachines, which can be empty or stale — e.g. when
// the CR is deleted before its first successful reconcile or after a partially
// failed one. status.AllocatedMachines remains informational only.
func (r *OmniClusterReconciler) deleteOmniResources(ctx context.Context, cluster *api.OmniCluster) error {
	clusterName := cluster.Name

	// Refuse to tear down an Omni cluster owned by a different OmniCluster CR
	// (same name, different namespace): its machine sets, patches and secrets
	// belong to that CR's lifecycle, not this one's.
	if owner, ok, err := r.OmniClient.ClusterOwner(ctx, clusterName); err != nil {
		return err
	} else if ok && owner != ownerOf(cluster) {
		log.FromContext(ctx).Info("WARNING: omni cluster is owned by another OmniCluster, skipping teardown",
			"cluster", clusterName, "owner", owner, "thisOmniCluster", ownerOf(cluster))
		return nil
	}

	// Delete the kubeconfig Secret(s) before tearing down Omni resources.
	// Both naming formats are attempted in case the --argocd-clusters flag
	// changed during the cluster's lifetime.
	for _, name := range []string{clusterName + "-kubeconfig", clusterName + "-cluster-secret"} {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.KubeconfigNamespace},
		}
		if err := client.IgnoreNotFound(r.Delete(ctx, secret)); err != nil {
			return fmt.Errorf("delete kubeconfig secret %s: %w", name, err)
		}
	}

	machineSetIDs, err := r.OmniClient.ListMachineSetIDsForCluster(ctx, clusterName)
	if err != nil {
		return fmt.Errorf("list machine sets for cluster %s: %w", clusterName, err)
	}

	// Remove all MachineSetNodes first (machines release back to available
	// pool), along with each machine's ConfigPatches and KernelArgs so the
	// machine doesn't carry stale customisation into the next cluster that
	// allocates it. DeleteMachineSetNode returns an error while teardown is in
	// progress, which makes controller-runtime requeue and retry.
	for _, machineSetID := range machineSetIDs {
		machines, err := r.OmniClient.AllocatedMachineSetNodes(ctx, machineSetID)
		if err != nil {
			return fmt.Errorf("list machine set nodes for %s: %w", machineSetID, err)
		}
		for _, machineID := range machines {
			if err := r.releaseMachine(ctx, clusterName, machineID); err != nil {
				return err
			}
		}
	}

	for _, machineSetID := range machineSetIDs {
		if err := r.OmniClient.DeleteExtensionsConfigurationForMachineSet(ctx, machineSetID); err != nil {
			return fmt.Errorf("delete extensions configuration for machine set %s: %w", machineSetID, err)
		}
		if err := r.OmniClient.DeleteMachineSet(ctx, machineSetID); err != nil {
			return fmt.Errorf("delete machine set %s: %w", machineSetID, err)
		}
	}

	if err := r.OmniClient.DeleteCluster(ctx, clusterName); err != nil {
		return err
	}
	r.eventf(cluster, corev1.EventTypeNormal, "ClusterDeleted", "Omni cluster resources deleted")
	return nil
}

// setFailure updates status with a failure reason and requeues. The write is
// skipped when status is unchanged from statusBefore so repeated failures do
// not retrigger reconcile through our own watch.
func (r *OmniClusterReconciler) setFailure(ctx context.Context, cluster *api.OmniCluster, statusBefore *api.OmniClusterStatus, reason string, err error) (ctrl.Result, error) {
	msg := err.Error()
	r.eventf(cluster, corev1.EventTypeWarning, reason, "%s", msg)
	cluster.Status.ObservedGeneration = cluster.Generation
	cluster.Status.Ready = false
	cluster.Status.FailureReason = &reason
	cluster.Status.FailureMessage = &msg
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: cluster.Generation,
	})
	if statusNeedsUpdate(statusBefore, &cluster.Status) {
		_ = r.Status().Update(ctx, cluster)
	}

	// Returning err alongside RequeueAfter would make controller-runtime discard
	// RequeueAfter and fall back to its exponential-backoff rate limiter instead
	// (and log a warning about it); the failure is already fully recorded above
	// via the Event and status condition, so return nil here to get the intended
	// fixed retry cadence.
	return ctrl.Result{RequeueAfter: requeueShort}, nil
}

// secretNeedsRefresh reports whether the kubeconfig Secret must be re-fetched
// from Omni: it is missing, lacks the expected data key, or its refresh
// annotation is absent, unparsable, or older than the refresh interval.
func secretNeedsRefresh(secret *corev1.Secret, dataKey string, now time.Time) bool {
	if secret == nil {
		return true
	}
	if _, ok := secret.Data[dataKey]; !ok {
		return true
	}
	refreshedAt, err := time.Parse(time.RFC3339, secret.Annotations[kubeconfigRefreshedAtAnnotation])
	if err != nil {
		return true
	}
	return now.Sub(refreshedAt) >= kubeconfigRefreshInterval
}

// ensureKubeconfigSecret fetches the cluster kubeconfig from Omni and writes it
// as a Secret for Flux remote cluster apply. The fetch is skipped while the
// existing Secret is younger than kubeconfigRefreshInterval, so a new
// service-account token is not minted on every reconcile. The returned bool
// reports whether the Secret was actually written (false on the skip path).
func (r *OmniClusterReconciler) ensureKubeconfigSecret(ctx context.Context, clusterName string) (bool, error) {
	now := time.Now()
	secretName := clusterName + "-kubeconfig"

	existing := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: r.KubeconfigNamespace, Name: secretName}, existing)
	if err != nil && !errors.IsNotFound(err) {
		return false, fmt.Errorf("get kubeconfig secret %s: %w", secretName, err)
	}
	if err == nil && !secretNeedsRefresh(existing, "value", now) {
		return false, nil
	}

	data, err := r.OmniClient.GetKubeconfig(ctx, clusterName)
	if err != nil {
		return false, err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: r.KubeconfigNamespace,
		},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		secret.Annotations[kubeconfigRefreshedAtAnnotation] = now.Format(time.RFC3339)
		secret.Data = map[string][]byte{"value": data}
		return nil
	})
	return err == nil, err
}

// ensureArgoCDClusterSecret writes the cluster kubeconfig as an ArgoCD cluster Secret.
// The fetch is skipped while the existing Secret is younger than
// kubeconfigRefreshInterval, so a new service-account token is not minted on
// every reconcile. The returned bool reports whether the Secret was actually
// written (false on the skip path).
func (r *OmniClusterReconciler) ensureArgoCDClusterSecret(ctx context.Context, clusterName string) (bool, error) {
	now := time.Now()
	secretName := clusterName + "-cluster-secret"

	existing := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: r.KubeconfigNamespace, Name: secretName}, existing)
	if err != nil && !errors.IsNotFound(err) {
		return false, fmt.Errorf("get argocd cluster secret %s: %w", secretName, err)
	}
	if err == nil && !secretNeedsRefresh(existing, "config", now) {
		return false, nil
	}

	data, err := r.OmniClient.GetKubeconfig(ctx, clusterName)
	if err != nil {
		return false, err
	}

	cfg, err := clientcmd.Load(data)
	if err != nil {
		return false, fmt.Errorf("parse kubeconfig: %w", err)
	}

	kctx, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok {
		return false, fmt.Errorf("context %q not found in kubeconfig", cfg.CurrentContext)
	}
	clusterEntry, ok := cfg.Clusters[kctx.Cluster]
	if !ok {
		return false, fmt.Errorf("cluster %q not found in kubeconfig", kctx.Cluster)
	}
	authInfo, ok := cfg.AuthInfos[kctx.AuthInfo]
	if !ok {
		return false, fmt.Errorf("user %q not found in kubeconfig", kctx.AuthInfo)
	}

	type tlsClientConfig struct {
		Insecure bool   `json:"insecure"`
		CAData   string `json:"caData,omitempty"`
	}
	type argoConfig struct {
		BearerToken     string          `json:"bearerToken"`
		TLSClientConfig tlsClientConfig `json:"tlsClientConfig"`
	}

	cfgJSON, err := json.Marshal(argoConfig{
		BearerToken: authInfo.Token,
		TLSClientConfig: tlsClientConfig{
			Insecure: false,
			CAData:   base64.StdEncoding.EncodeToString(clusterEntry.CertificateAuthorityData),
		},
	})
	if err != nil {
		return false, fmt.Errorf("marshal argocd config: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: r.KubeconfigNamespace,
		},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		secret.Labels["argocd.argoproj.io/secret-type"] = "cluster"
		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		secret.Annotations[kubeconfigRefreshedAtAnnotation] = now.Format(time.RFC3339)
		secret.Data = map[string][]byte{
			"name":   []byte(clusterName),
			"server": []byte(clusterEntry.Server),
			"config": cfgJSON,
		}
		return nil
	})
	return err == nil, err
}

func (r *OmniClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&api.OmniCluster{}).
		Complete(r)
}

// ── helpers ───────────────────────────────────────────────────────────────────

// ownerOf returns the owner identity stamped on Omni Cluster resources for
// this CR: "<namespace>/<name>".
func ownerOf(cluster *api.OmniCluster) string {
	return cluster.Namespace + "/" + cluster.Name
}

// selectRebootCandidate returns the first drifting machine whose last
// drift-remediation reboot is absent or older than rebootCooldown, or nil if
// every drifting machine is still cooling down.
func selectRebootCandidate(drifting []MachineConfigDrift, lastReboots map[string]metav1.Time, now time.Time) *MachineConfigDrift {
	for i := range drifting {
		last, ok := lastReboots[drifting[i].MachineID]
		if !ok || now.Sub(last.Time) >= rebootCooldown {
			return &drifting[i]
		}
	}
	return nil
}

// statusNeedsUpdate reports whether the status changed during this reconcile
// and therefore needs to be written back to the API server. Skipping no-op
// writes matters because the controller watches its own CR: every status
// write fires a watch event that retriggers reconcile immediately.
func statusNeedsUpdate(before, after *api.OmniClusterStatus) bool {
	return !equality.Semantic.DeepEqual(before, after)
}

func boolToConditionStatus(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}
