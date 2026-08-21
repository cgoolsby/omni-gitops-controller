package controllers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	goerrors "errors"
	"fmt"
	"sort"
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

// OmniClusterReconciler reconciles OmniCluster objects. It owns cluster- and
// machine-set-scoped Omni state (Cluster + MachineSet existence, extensions /
// schematic config, scale-up/down, cross-machine drift detection and
// reboot-candidate selection) and expresses per-machine allocation by
// creating/updating/deleting Machine objects, which the MachineReconciler
// converges onto Omni. It never touches per-machine Omni state directly.
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

	// claimed accumulates every machine UUID already spoken for by a Machine
	// object this cluster owns (across all sets), so a machine selected for one
	// set earlier this reconcile is never re-selected for another before its
	// Machine object is bound in Omni.
	claimed := map[string]bool{}
	allocatedMachines := map[string][]string{}
	if result, stop := r.allocateControlPlaneMachines(ctx, cluster, statusBefore, clusterName, allocatedMachines, claimed); stop {
		return result, nil
	}
	if result, stop := r.allocateWorkerMachines(ctx, cluster, statusBefore, clusterName, allocatedMachines, claimed); stop {
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

	// ── Signal drift-remediation reboots ──────────────────────────────────────
	// The cluster controller owns cross-machine reboot-candidate selection (it
	// needs cluster-wide visibility for the "one reboot in flight" invariant),
	// but the reboot itself is owned by the Machine controller: here we only set
	// Spec.RebootRequestedAt on the chosen Machine and let it do the RPC.
	if omniStatus.Ready {
		if err := r.signalDriftReboots(ctx, cluster, clusterName, drifting); err != nil {
			return r.setFailure(ctx, cluster, statusBefore, "SignalRebootFailed", err)
		}
	}

	// Every Omni-side action this reconcile needs to perform (kubeconfig) runs
	// here, before the single status write below, so that write always reflects
	// this pass's fully settled outcome.
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

	logger.Info("Cluster reconciled", "phase", omniStatus.Phase)
	return ctrl.Result{RequeueAfter: requeueLong}, nil
}

// signalDriftReboots picks at most one drifting machine cluster-wide to reboot
// and requests it by setting Spec.RebootRequestedAt on that Machine. It enforces
// the "one reboot in flight cluster-wide" invariant (skip if any machine already
// has a reboot owed) and the per-machine cooldown (skip a machine rebooted
// within rebootCooldown). GetDriftingMachines already returns nothing while any
// machine in the cluster is unhealthy, so a machine still mid-reboot also blocks
// new candidates. Sets the ConfigDriftDetected condition to match the outcome.
func (r *OmniClusterReconciler) signalDriftReboots(ctx context.Context, cluster *api.OmniCluster, clusterName string, drifting []MachineConfigDrift) error {
	if len(drifting) == 0 {
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               "ConfigDriftDetected",
			Status:             metav1.ConditionFalse,
			Reason:             "AllMachinesUpToDate",
			Message:            "All machines are running the target config",
			ObservedGeneration: cluster.Generation,
		})
		return nil
	}

	machines, err := r.listClusterMachines(ctx, cluster.Namespace, clusterName)
	if err != nil {
		return err
	}
	byID := make(map[string]*api.Machine, len(machines))
	for i := range machines {
		byID[machines[i].Name] = &machines[i]
	}

	// A reboot already requested but not yet stamped (owed) is one in flight; do
	// not request a second while it is pending.
	if anyRebootInFlight(machines) {
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               "ConfigDriftDetected",
			Status:             metav1.ConditionTrue,
			Reason:             "PendingReboot",
			Message:            "A drift-remediation reboot is already in flight",
			ObservedGeneration: cluster.Generation,
		})
		return nil
	}

	now := metav1.Now()
	candidate := selectDriftRebootCandidate(drifting, byID, now.Time)
	if candidate == nil {
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
		return nil
	}

	m := byID[candidate.MachineID]
	m.Spec.RebootRequestedAt = &now
	m.Spec.ManagementAddress = candidate.ManagementAddress
	if err := r.Update(ctx, m); err != nil {
		return fmt.Errorf("request reboot for machine %s: %w", candidate.MachineID, err)
	}
	r.eventf(cluster, corev1.EventTypeNormal, "RebootRequested",
		"Requested reboot of machine %s to apply config update", candidate.MachineID)
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               "ConfigDriftDetected",
		Status:             metav1.ConditionTrue,
		Reason:             "PendingReboot",
		Message:            fmt.Sprintf("Machine %s has pending config update; reboot requested", candidate.MachineID),
		ObservedGeneration: cluster.Generation,
	})
	return nil
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
	claimed map[string]bool,
) (ctrl.Result, bool) {
	cpMachineSetID := omnires.ControlPlanesResourceID(clusterName)
	if err := r.OmniClient.EnsureMachineSet(ctx, clusterName, cpMachineSetID, omnires.LabelControlPlaneRole); err != nil {
		result, _ := r.setFailure(ctx, cluster, statusBefore, "EnsureMachineSetFailed", err)
		return result, true
	}

	allocated, err := r.reconcileMachineSetAllocation(ctx, cluster, clusterName, cpMachineSetID,
		roleControlPlane, cluster.Spec.ControlPlane, claimed)
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
	claimed map[string]bool,
) (ctrl.Result, bool) {
	for _, w := range cluster.Spec.Workers {
		wID := fmt.Sprintf("%s-%s", clusterName, w.Name)
		if err := r.OmniClient.EnsureMachineSet(ctx, clusterName, wID, omnires.LabelWorkerRole); err != nil {
			result, _ := r.setFailure(ctx, cluster, statusBefore, "EnsureWorkerMachineSetFailed", err)
			return result, true
		}
		allocated, err := r.reconcileMachineSetAllocation(ctx, cluster, clusterName, wID,
			roleWorker, w.MachineSetSpec, claimed)
		if err != nil {
			result, _ := r.setFailure(ctx, cluster, statusBefore, "AllocateWorkerMachinesFailed", err)
			return result, true
		}
		allocatedMachines[wID] = allocated
	}
	return ctrl.Result{}, false
}

// reconcileMachineSetAllocation converges the set of Machine objects for one
// machine set onto spec.Replicas: it scales up by selecting available Omni
// machines and creating Machine objects, scales down by deleting them (one at a
// time for the control plane, to protect etcd quorum), keeps already-allocated
// Machine specs in sync with the machine-set spec, and reconciles the
// machine-set-scoped extensions configuration. It never touches per-machine Omni
// state directly — the MachineReconciler converges each Machine onto Omni.
// Returns the machine UUIDs currently allocated to (kept in) this set.
func (r *OmniClusterReconciler) reconcileMachineSetAllocation(
	ctx context.Context,
	cluster *api.OmniCluster,
	clusterName, machineSetID, role string,
	spec api.MachineSetSpec,
	claimed map[string]bool,
) ([]string, error) {
	existing, err := r.listMachineSetMachines(ctx, cluster.Namespace, clusterName, machineSetID)
	if err != nil {
		return nil, fmt.Errorf("list machines for %s: %w", machineSetID, err)
	}

	// ── Adopt pre-existing Omni bindings ────────────────────────────────────────
	// Allocation is Omni-authoritative: a machine already bound to this set in
	// Omni but lacking a Machine object (e.g. a cluster created before the Machine
	// CRD existed) must be adopted, not treated as absent. Without this the
	// "already allocated" baseline would read as 0 and we would scale up a *second*
	// machine on top of the live one — the v0.3.0/v0.3.1 double-bind regression.
	bound, err := r.OmniClient.AllocatedMachineSetNodes(ctx, machineSetID)
	if err != nil {
		return nil, fmt.Errorf("list omni bindings for %s: %w", machineSetID, err)
	}
	haveObject := make(map[string]bool, len(existing))
	for i := range existing {
		haveObject[existing[i].Name] = true
	}
	for _, machineID := range bound {
		if haveObject[machineID] {
			continue
		}
		// createMachine fronts the existing MachineSetNode with a Machine carrying
		// the same owner ref and labels. It does NOT create a second binding: the
		// MachineReconciler's EnsureMachineSetNode is idempotent for a UUID already
		// bound to this set.
		if err := r.createMachine(ctx, cluster, clusterName, machineSetID, role, machineID, spec); err != nil {
			return nil, fmt.Errorf("adopt machine %s into %s: %w", machineID, machineSetID, err)
		}
		haveObject[machineID] = true
		// Append a synthetic entry (mirroring the scale-up path) so this adopted
		// machine is counted in `current` without a re-List that a stale cache
		// might not yet reflect. Zero ResourceVersion makes the sync loop skip it.
		existing = append(existing, api.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: machineID, Namespace: cluster.Namespace},
		})
		r.eventf(cluster, corev1.EventTypeNormal, "MachineAdopted",
			"Adopted pre-existing Omni binding %s into %s", machineID, machineSetID)
	}

	// Every machine that already has an object (even one mid-deletion) is
	// claimed, so it is never re-selected for another set this reconcile.
	var active []api.Machine
	deleting := 0
	for i := range existing {
		claimed[existing[i].Name] = true
		if existing[i].DeletionTimestamp.IsZero() {
			active = append(active, existing[i])
		} else {
			deleting++
		}
	}

	desired := int(spec.Replicas)
	current := len(active)

	switch {
	case current < desired:
		// ── Scale up ──────────────────────────────────────────────────────────
		needed := desired - current
		candidates, err := r.OmniClient.SelectAvailableMachines(ctx, spec.MachineSelector, needed+len(claimed))
		if err != nil {
			return nil, fmt.Errorf("select machines for %s: %w", machineSetID, err)
		}

		var fresh []string
		for _, id := range candidates {
			if claimed[id] {
				continue
			}
			fresh = append(fresh, id)
			if len(fresh) >= needed {
				break
			}
		}
		if len(fresh) < needed {
			return nil, fmt.Errorf("not enough available machines for %s: need %d more, found %d candidates",
				machineSetID, needed, len(fresh))
		}

		for _, machineID := range fresh {
			if err := r.createMachine(ctx, cluster, clusterName, machineSetID, role, machineID, spec); err != nil {
				return nil, fmt.Errorf("create machine %s for %s: %w", machineID, machineSetID, err)
			}
			claimed[machineID] = true
			active = append(active, api.Machine{ObjectMeta: metav1.ObjectMeta{Name: machineID}})
		}
		r.eventf(cluster, corev1.EventTypeNormal, "MachinesAllocated",
			"Allocated %d machine(s) to %s", len(fresh), machineSetID)

	case current > desired:
		// ── Scale down ────────────────────────────────────────────────────────
		toRemove := current - desired

		// Order removal candidates unhealthy-first (not connected / not ready),
		// ties broken by name for determinism, so scale-down sheds the safest
		// machines first. Health missing from Omni counts as unhealthy.
		health, err := r.OmniClient.MachineSetNodeHealth(ctx, machineSetID)
		if err != nil {
			return nil, fmt.Errorf("machine health for %s: %w", machineSetID, err)
		}
		sort.SliceStable(active, func(i, j int) bool {
			hi, hj := machineHealthy(health, active[i].Name), machineHealthy(health, active[j].Name)
			if hi != hj {
				return !hi // unhealthy (false) sorts before healthy (true)
			}
			return active[i].Name < active[j].Name
		})

		if role == roleControlPlane {
			if desired < 1 {
				return nil, fmt.Errorf("control plane cannot scale below 1 replica")
			}
			if desired%2 == 0 {
				log.FromContext(ctx).Info("WARNING: even number of control-plane replicas loses etcd quorum margin",
					"replicas", desired)
			}
			// Remove at most one control-plane member per reconcile, and never
			// start a new removal while one is still tearing down: evicting
			// several etcd members at once risks quorum loss. The periodic
			// RequeueAfter and the .Owns watch pick up the next removal once the
			// prior Machine's finalizer has released it.
			switch {
			case deleting > 0:
				toRemove = 0
			case toRemove > 1:
				toRemove = 1
			}
			// Refuse to auto-remove a healthy control-plane member. After the
			// unhealthy-first sort, if the best candidate is still healthy then all
			// members are healthy and the choice of which etcd member to drop is
			// ambiguous — removing one unattended risks quorum. Leave it to an
			// operator and requeue via the periodic RequeueAfter.
			if toRemove > 0 && machineHealthy(health, active[0].Name) {
				log.FromContext(ctx).Info("WARNING: control plane over-allocated but all members healthy; refusing to auto-remove an etcd member",
					"set", machineSetID, "current", current, "desired", desired)
				r.eventf(cluster, corev1.EventTypeWarning, "ControlPlaneOverAllocated",
					"Control plane %s has %d members but desires %d; all are healthy, so removing one unattended risks etcd quorum — remove a member manually",
					machineSetID, current, desired)
				toRemove = 0
			}
		}

		if toRemove > 0 {
			evict := active[:toRemove]
			active = active[toRemove:]
			for i := range evict {
				if err := r.deleteMachine(ctx, &evict[i]); err != nil {
					return nil, fmt.Errorf("evict machine %s from %s: %w", evict[i].Name, machineSetID, err)
				}
			}
			r.eventf(cluster, corev1.EventTypeNormal, "MachinesReleased",
				"Released %d machine(s) from %s", len(evict), machineSetID)
		}
	}

	// Keep already-allocated Machine specs in sync with the machine-set spec so
	// config-patch / kernel-arg / role changes propagate to existing machines.
	// Freshly-created machines (zero ResourceVersion) already carry the spec.
	for i := range active {
		if active[i].ResourceVersion == "" {
			continue
		}
		if err := r.syncMachineSpec(ctx, &active[i], clusterName, machineSetID, role, spec); err != nil {
			return nil, fmt.Errorf("sync machine %s spec: %w", active[i].Name, err)
		}
	}

	// Reconcile extensions configuration at machine-set level (stays cluster-
	// scoped in Omni). Delete the resource when the list is cleared.
	if len(spec.MachineExtensions) > 0 {
		if err := r.OmniClient.EnsureMachineSetExtensionsConfiguration(ctx, clusterName, machineSetID, spec.MachineExtensions); err != nil {
			return nil, fmt.Errorf("extensions configuration for %s: %w", machineSetID, err)
		}
	} else {
		if err := r.OmniClient.DeleteExtensionsConfigurationForMachineSet(ctx, machineSetID); err != nil {
			return nil, fmt.Errorf("delete extensions configuration for %s: %w", machineSetID, err)
		}
	}

	allocated := make([]string, 0, len(active))
	for i := range active {
		allocated = append(allocated, active[i].Name)
	}
	return allocated, nil
}

// createMachine creates a Machine object for a freshly-allocated machine, owned
// (controller reference) by the cluster so it is garbage-collected on cluster
// delete and its state changes enqueue the owner via the .Owns watch.
func (r *OmniClusterReconciler) createMachine(
	ctx context.Context,
	cluster *api.OmniCluster,
	clusterName, machineSetID, role, machineID string,
	spec api.MachineSetSpec,
) error {
	m := &api.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      machineID,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				labelCluster:    clusterName,
				labelMachineSet: machineSetID,
			},
		},
		Spec: desiredMachineSpec(clusterName, machineSetID, role, spec),
	}
	if err := controllerutil.SetControllerReference(cluster, m, r.Scheme); err != nil {
		return fmt.Errorf("set controller reference: %w", err)
	}
	if err := r.Create(ctx, m); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// syncMachineSpec updates an existing Machine's spec to match the machine-set
// spec, preserving the reboot-signaling fields (RebootRequestedAt,
// ManagementAddress) which are owned by the drift-reboot path, not by the set.
func (r *OmniClusterReconciler) syncMachineSpec(
	ctx context.Context,
	machine *api.Machine,
	clusterName, machineSetID, role string,
	spec api.MachineSetSpec,
) error {
	updated := machine.DeepCopy()
	want := desiredMachineSpec(clusterName, machineSetID, role, spec)
	updated.Spec.ClusterName = want.ClusterName
	updated.Spec.MachineSetID = want.MachineSetID
	updated.Spec.Role = want.Role
	updated.Spec.ConfigPatches = want.ConfigPatches
	updated.Spec.KernelArgs = want.KernelArgs
	if updated.Labels == nil {
		updated.Labels = map[string]string{}
	}
	updated.Labels[labelCluster] = clusterName
	updated.Labels[labelMachineSet] = machineSetID

	if equality.Semantic.DeepEqual(machine.Spec, updated.Spec) &&
		equality.Semantic.DeepEqual(machine.Labels, updated.Labels) {
		return nil
	}
	return r.Update(ctx, updated)
}

// pruneOrphanedMachineSets tears down Omni MachineSets labelled with this cluster
// that are no longer declared in spec — e.g. when a user removes a worker set
// from spec.workers without deleting the whole OmniCluster. It deletes the
// Machine objects for each orphaned set (letting their finalizers unwind the
// per-machine Omni state) and, once they are gone, deletes the machine-set-scoped
// Omni resources. Discovery is driven by Omni state (the authoritative source)
// rather than status.AllocatedMachines, which can be empty or stale.
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

		// Delete the Machine objects for this set and wait (via requeue + the
		// .Owns watch) for their finalizers to unwind per-machine Omni state
		// before removing the machine-set-scoped resources.
		remaining, err := r.deleteMachineSetMachines(ctx, cluster, clusterName, machineSetID)
		if err != nil {
			return err
		}
		if remaining > 0 {
			return fmt.Errorf("waiting for %d machine(s) in orphaned set %s to finish teardown", remaining, machineSetID)
		}

		// Backstop: release any Omni MachineSetNodes not fronted by a Machine
		// object (e.g. leaked or created out-of-band), so the set can be removed.
		nodes, err := r.OmniClient.AllocatedMachineSetNodes(ctx, machineSetID)
		if err != nil {
			return fmt.Errorf("list machine set nodes for %s: %w", machineSetID, err)
		}
		for _, machineID := range nodes {
			if err := releaseMachineOmni(ctx, r.OmniClient, clusterName, machineID); err != nil {
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

// deleteOmniResources tears down all resources for this cluster on delete. It
// first deletes the Machine objects (whose finalizers unwind per-machine Omni
// state) and waits for them to be gone, then removes the machine-set-scoped and
// cluster-scoped Omni resources. Discovery of the latter is driven by Omni state
// (the authoritative source) rather than status.AllocatedMachines, which can be
// empty or stale — e.g. when the CR is deleted before its first successful
// reconcile or after a partially failed one.
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

	// Delete every Machine object this cluster owns and wait for their finalizers
	// to unwind per-machine Omni state (unbind, config patches, kernel args)
	// before tearing down the Omni-side machine sets/cluster.
	machines, err := r.listClusterMachines(ctx, cluster.Namespace, clusterName)
	if err != nil {
		return fmt.Errorf("list machines for cluster %s: %w", clusterName, err)
	}
	for i := range machines {
		if err := r.deleteMachine(ctx, &machines[i]); err != nil {
			return fmt.Errorf("delete machine %s: %w", machines[i].Name, err)
		}
	}
	if len(machines) > 0 {
		return fmt.Errorf("waiting for %d machine(s) to finish teardown", len(machines))
	}

	machineSetIDs, err := r.OmniClient.ListMachineSetIDsForCluster(ctx, clusterName)
	if err != nil {
		return fmt.Errorf("list machine sets for cluster %s: %w", clusterName, err)
	}

	// Backstop: release any MachineSetNodes not fronted by a Machine object
	// (leaked or created out-of-band). In normal operation the Machine
	// finalizers above have already unbound every node.
	for _, machineSetID := range machineSetIDs {
		nodes, err := r.OmniClient.AllocatedMachineSetNodes(ctx, machineSetID)
		if err != nil {
			return fmt.Errorf("list machine set nodes for %s: %w", machineSetID, err)
		}
		for _, machineID := range nodes {
			if err := releaseMachineOmni(ctx, r.OmniClient, clusterName, machineID); err != nil {
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
		Owns(&api.Machine{}).
		Complete(r)
}

// ── Machine object helpers ──────────────────────────────────────────────────

// listClusterMachines returns all Machine objects owned by this cluster (across
// all machine sets), found by the cluster label instead of a bookkeeping map.
func (r *OmniClusterReconciler) listClusterMachines(ctx context.Context, namespace, clusterName string) ([]api.Machine, error) {
	list := &api.MachineList{}
	if err := r.List(ctx, list,
		client.InNamespace(namespace),
		client.MatchingLabels{labelCluster: clusterName},
	); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// listMachineSetMachines returns the Machine objects allocated to one set.
func (r *OmniClusterReconciler) listMachineSetMachines(ctx context.Context, namespace, clusterName, machineSetID string) ([]api.Machine, error) {
	list := &api.MachineList{}
	if err := r.List(ctx, list,
		client.InNamespace(namespace),
		client.MatchingLabels{labelCluster: clusterName, labelMachineSet: machineSetID},
	); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// deleteMachine deletes a Machine object, tolerating one already gone or already
// being deleted.
func (r *OmniClusterReconciler) deleteMachine(ctx context.Context, machine *api.Machine) error {
	if !machine.DeletionTimestamp.IsZero() {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, machine))
}

// deleteMachineSetMachines deletes every Machine object in a set and returns how
// many still exist (including any still tearing down via their finalizer).
func (r *OmniClusterReconciler) deleteMachineSetMachines(ctx context.Context, cluster *api.OmniCluster, clusterName, machineSetID string) (int, error) {
	machines, err := r.listMachineSetMachines(ctx, cluster.Namespace, clusterName, machineSetID)
	if err != nil {
		return 0, fmt.Errorf("list machines for %s: %w", machineSetID, err)
	}
	for i := range machines {
		if err := r.deleteMachine(ctx, &machines[i]); err != nil {
			return 0, fmt.Errorf("delete machine %s: %w", machines[i].Name, err)
		}
	}
	return len(machines), nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// ownerOf returns the owner identity stamped on Omni Cluster resources for
// this CR: "<namespace>/<name>".
func ownerOf(cluster *api.OmniCluster) string {
	return cluster.Namespace + "/" + cluster.Name
}

// desiredMachineSpec builds the machine-set-derived portion of a Machine spec.
// It deliberately leaves RebootRequestedAt/ManagementAddress zero: those are
// owned by the drift-reboot signaling path, not by the machine set.
func desiredMachineSpec(clusterName, machineSetID, role string, spec api.MachineSetSpec) api.MachineSpec {
	return api.MachineSpec{
		ClusterName:   clusterName,
		MachineSetID:  machineSetID,
		Role:          role,
		ConfigPatches: spec.ConfigPatches,
		KernelArgs:    spec.KernelArgs,
	}
}

// machineHealthy reports whether a machine is both connected and cluster-ready
// per Omni. A machine absent from the health map (no Omni status yet) is treated
// as unhealthy, making it a preferred scale-down candidate.
func machineHealthy(health map[string]MachineHealth, id string) bool {
	h, ok := health[id]
	return ok && h.Connected && h.Ready
}

// anyRebootInFlight reports whether any machine has a reboot owed (requested but
// not yet stamped), i.e. a drift-remediation reboot is already in flight.
func anyRebootInFlight(machines []api.Machine) bool {
	for i := range machines {
		if rebootOwed(machines[i].Spec, machines[i].Status) {
			return true
		}
	}
	return false
}

// selectDriftRebootCandidate returns the first drifting machine whose backing
// Machine object's last reboot is absent or older than rebootCooldown, or nil if
// every drifting machine is still cooling down (or has no Machine object).
func selectDriftRebootCandidate(drifting []MachineConfigDrift, byID map[string]*api.Machine, now time.Time) *MachineConfigDrift {
	for i := range drifting {
		m := byID[drifting[i].MachineID]
		if m == nil {
			continue
		}
		last := m.Status.LastRebootTime
		if last == nil || now.Sub(last.Time) >= rebootCooldown {
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
