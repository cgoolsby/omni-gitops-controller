package controllers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	api "github.com/cgoolsby/omni-gitops-controller/api/v1alpha1"
	omnires "github.com/siderolabs/omni/client/pkg/omni/resources/omni"
)

const (
	finalizerName = "omnicluster.omni.gitops.dev/finalizer"
	requeueShort  = 15 * time.Second
	requeueLong   = 60 * time.Second
)

// OmniClusterReconciler reconciles OmniCluster objects.
type OmniClusterReconciler struct {
	client.Client
	Scheme              *runtime.Scheme
	OmniClient          *OmniClient
	KubeconfigNamespace string
	ArgoCDClusters      bool
}

// +kubebuilder:rbac:groups=omni.gitops.dev,resources=omniclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=omni.gitops.dev,resources=omniclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=omni.gitops.dev,resources=omniclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update;patch

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

	// ── Reconcile Omni resources ──────────────────────────────────────────────
	clusterName := cluster.Name

	if err := r.OmniClient.EnsureCluster(ctx,
		clusterName,
		cluster.Spec.KubernetesVersion,
		cluster.Spec.TalosVersion,
	); err != nil {
		return r.setFailure(ctx, cluster, "EnsureClusterFailed", err)
	}

	cpMachineSetID := omnires.ControlPlanesResourceID(clusterName)
	if err := r.OmniClient.EnsureMachineSet(ctx, clusterName, cpMachineSetID, omnires.LabelControlPlaneRole); err != nil {
		return r.setFailure(ctx, cluster, "EnsureMachineSetFailed", err)
	}

	allocatedMachines := map[string][]string{}

	// Allocate control plane machines.
	if allocated, err := r.reconcileMachineSet(ctx, cluster, clusterName, cpMachineSetID,
		omnires.LabelControlPlaneRole, cluster.Spec.ControlPlane); err != nil {
		return r.setFailure(ctx, cluster, "AllocateCPMachinesFailed", err)
	} else {
		allocatedMachines[cpMachineSetID] = allocated
	}

	// Allocate worker machines.
	for _, w := range cluster.Spec.Workers {
		wID := fmt.Sprintf("%s-%s", clusterName, w.Name)
		if err := r.OmniClient.EnsureMachineSet(ctx, clusterName, wID, omnires.LabelWorkerRole); err != nil {
			return r.setFailure(ctx, cluster, "EnsureWorkerMachineSetFailed", err)
		}
		if allocated, err := r.reconcileMachineSet(ctx, cluster, clusterName, wID,
			omnires.LabelWorkerRole, w.MachineSetSpec); err != nil {
			return r.setFailure(ctx, cluster, "AllocateWorkerMachinesFailed", err)
		} else {
			allocatedMachines[wID] = allocated
		}
	}

	// ── Update status from Omni ───────────────────────────────────────────────
	omniStatus, err := r.OmniClient.GetClusterStatus(ctx, clusterName)
	if err != nil {
		return r.setFailure(ctx, cluster, "GetStatusFailed", err)
	}

	// Detect config drift only when the cluster is ready (machines must be Running).
	var drifting []MachineConfigDrift
	if omniStatus.Ready {
		drifting, err = r.OmniClient.GetDriftingMachines(ctx, clusterName)
		if err != nil {
			return r.setFailure(ctx, cluster, "ConfigDriftCheckFailed", err)
		}
	}

	cluster.Status.Phase = omniStatus.Phase
	cluster.Status.Ready = omniStatus.Ready
	cluster.Status.AllocatedMachines = allocatedMachines
	cluster.Status.FailureReason = nil
	cluster.Status.FailureMessage = nil
	setCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             boolToConditionStatus(omniStatus.Ready),
		Reason:             omniStatus.Phase,
		Message:            fmt.Sprintf("Omni cluster phase: %s", omniStatus.Phase),
		LastTransitionTime: metav1.Now(),
	})

	if len(drifting) == 0 {
		setCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               "ConfigDriftDetected",
			Status:             metav1.ConditionFalse,
			Reason:             "AllMachinesUpToDate",
			Message:            "All machines are running the target config",
			LastTransitionTime: metav1.Now(),
		})
	} else {
		setCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               "ConfigDriftDetected",
			Status:             metav1.ConditionTrue,
			Reason:             "PendingReboot",
			Message:            fmt.Sprintf("Machine %s has pending config update; reboot scheduled", drifting[0].MachineID),
			LastTransitionTime: metav1.Now(),
		})
	}

	if err := r.Status().Update(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}

	if !omniStatus.Ready {
		logger.Info("Cluster not yet ready, requeuing", "phase", omniStatus.Phase)
		return ctrl.Result{RequeueAfter: requeueShort}, nil
	}

	var kubeconfigErr error
	if r.ArgoCDClusters {
		kubeconfigErr = r.ensureArgoCDClusterSecret(ctx, clusterName)
	} else {
		kubeconfigErr = r.ensureKubeconfigSecret(ctx, clusterName)
	}
	if kubeconfigErr != nil {
		return r.setFailure(ctx, cluster, "EnsureKubeconfigFailed", kubeconfigErr)
	}

	// Reboot the first drifting machine (rolling window = 1, prevents simultaneous reboots in HA clusters).
	if len(drifting) > 0 {
		first := drifting[0]
		logger.Info("Config drift detected, triggering reboot", "machine", first.MachineID, "addr", first.ManagementAddress)
		if err := r.OmniClient.RebootMachine(ctx, clusterName, first.ManagementAddress); err != nil {
			logger.Error(err, "Failed to reboot machine", "machine", first.MachineID)
		}
		return ctrl.Result{RequeueAfter: requeueLong}, nil
	}

	logger.Info("Cluster reconciled", "phase", omniStatus.Phase)
	return ctrl.Result{RequeueAfter: requeueLong}, nil
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
		}

		evict := already[len(already)-toRemove:]
		keep := already[:len(already)-toRemove]

		for _, machineID := range evict {
			if err := r.OmniClient.DeleteMachineSetNode(ctx, machineID); err != nil {
				return nil, fmt.Errorf("remove machine %s from %s: %w", machineID, machineSetID, err)
			}
			if err := r.OmniClient.DeleteConfigPatchesForMachine(ctx, clusterName, machineID); err != nil {
				return nil, fmt.Errorf("cleanup patches for evicted machine %s: %w", machineID, err)
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

	// Apply extensions configuration at the machine-set level (one resource covers all machines in the set).
	if len(spec.MachineExtensions) > 0 {
		if err := r.OmniClient.EnsureMachineSetExtensionsConfiguration(ctx, clusterName, machineSetID, spec.MachineExtensions); err != nil {
			return nil, fmt.Errorf("extensions configuration for %s: %w", machineSetID, err)
		}
	}

	return already, nil
}

// applyConfigPatches creates or updates Omni ConfigPatches for a machine.
func (r *OmniClusterReconciler) applyConfigPatches(
	ctx context.Context,
	clusterName, machineSetID, machineID string,
	patches []api.ConfigPatch,
) error {
	for _, p := range patches {
		patchID := fmt.Sprintf("%s-%s-%s", clusterName, machineID, p.Name)
		if err := r.OmniClient.EnsureConfigPatch(ctx, patchID, clusterName, machineSetID, machineID, json.RawMessage(p.Inline.Raw)); err != nil {
			return fmt.Errorf("ensure patch %q: %w", p.Name, err)
		}
	}
	return nil
}

// deleteOmniResources tears down all Omni resources for this cluster in reverse order.
func (r *OmniClusterReconciler) deleteOmniResources(ctx context.Context, cluster *api.OmniCluster) error {
	clusterName := cluster.Name

	// Remove all MachineSetNodes first (machines release back to available pool).
	for machineSetID, machines := range cluster.Status.AllocatedMachines {
		for _, machineID := range machines {
			if err := r.OmniClient.DeleteMachineSetNode(ctx, machineID); err != nil {
				return fmt.Errorf("delete machine set node %s: %w", machineID, err)
			}
		}
		if err := r.OmniClient.DeleteMachineSet(ctx, machineSetID); err != nil {
			return fmt.Errorf("delete machine set %s: %w", machineSetID, err)
		}
	}

	return r.OmniClient.DeleteCluster(ctx, clusterName)
}

// setFailure updates status with a failure reason and requeues.
func (r *OmniClusterReconciler) setFailure(ctx context.Context, cluster *api.OmniCluster, reason string, err error) (ctrl.Result, error) {
	msg := err.Error()
	cluster.Status.Ready = false
	cluster.Status.FailureReason = &reason
	cluster.Status.FailureMessage = &msg
	setCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
	})
	_ = r.Status().Update(ctx, cluster)
	return ctrl.Result{RequeueAfter: requeueShort}, err
}

// ensureKubeconfigSecret fetches the cluster kubeconfig from Omni and writes it
// as a Secret for Flux remote cluster apply.
func (r *OmniClusterReconciler) ensureKubeconfigSecret(ctx context.Context, clusterName string) error {
	data, err := r.OmniClient.GetKubeconfig(ctx, clusterName)
	if err != nil {
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName + "-kubeconfig",
			Namespace: r.KubeconfigNamespace,
		},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Data = map[string][]byte{"value": data}
		return nil
	})
	return err
}

// ensureArgoCDClusterSecret writes the cluster kubeconfig as an ArgoCD cluster Secret.
func (r *OmniClusterReconciler) ensureArgoCDClusterSecret(ctx context.Context, clusterName string) error {
	data, err := r.OmniClient.GetKubeconfig(ctx, clusterName)
	if err != nil {
		return err
	}

	cfg, err := clientcmd.Load(data)
	if err != nil {
		return fmt.Errorf("parse kubeconfig: %w", err)
	}

	kctx, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok {
		return fmt.Errorf("context %q not found in kubeconfig", cfg.CurrentContext)
	}
	clusterEntry, ok := cfg.Clusters[kctx.Cluster]
	if !ok {
		return fmt.Errorf("cluster %q not found in kubeconfig", kctx.Cluster)
	}
	authInfo, ok := cfg.AuthInfos[kctx.AuthInfo]
	if !ok {
		return fmt.Errorf("user %q not found in kubeconfig", kctx.AuthInfo)
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
		return fmt.Errorf("marshal argocd config: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName + "-cluster-secret",
			Namespace: r.KubeconfigNamespace,
		},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		secret.Labels["argocd.argoproj.io/secret-type"] = "cluster"
		secret.Data = map[string][]byte{
			"name":   []byte(clusterName),
			"server": []byte(clusterEntry.Server),
			"config": cfgJSON,
		}
		return nil
	})
	return err
}

func (r *OmniClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&api.OmniCluster{}).
		Complete(r)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func setCondition(conditions *[]metav1.Condition, cond metav1.Condition) {
	for i, c := range *conditions {
		if c.Type == cond.Type {
			(*conditions)[i] = cond
			return
		}
	}
	*conditions = append(*conditions, cond)
}

func boolToConditionStatus(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}
