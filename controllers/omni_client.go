package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/gen/pair"
	omniSpecs "github.com/siderolabs/omni/client/api/omni/specs"
	omniv1 "github.com/siderolabs/omni/client/pkg/client"
	"github.com/siderolabs/omni/client/pkg/client/management"
	omniresources "github.com/siderolabs/omni/client/pkg/omni/resources"
	omnires "github.com/siderolabs/omni/client/pkg/omni/resources/omni"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OmniClient wraps omni-client state for cluster lifecycle operations.
type OmniClient struct {
	state state.State
	mgmt  *management.Client
	raw   *omniv1.Client
}

// NewOmniClient builds a client authenticating with a service account key.
//
//	endpoint            - Omni HTTPS URL, e.g. "https://omni.192.168.1.92.nip.io:32080"
//	serviceAccountToken - base64-encoded service account key from `omnictl serviceaccount create`
func NewOmniClient(_ context.Context, endpoint, serviceAccountToken string) (*OmniClient, error) {
	c, err := omniv1.New(endpoint, omniv1.WithServiceAccount(serviceAccountToken))
	if err != nil {
		return nil, fmt.Errorf("omni client: %w", err)
	}
	return &OmniClient{state: c.Omni().State(), mgmt: c.Management(), raw: c}, nil
}

// MachineConfigDrift holds machine identity for a node whose running config is behind the desired config.
type MachineConfigDrift struct {
	MachineID         string
	ManagementAddress string
}

// GetDriftingMachines returns machines in the cluster whose running config does not match the desired
// config (ConfigUpToDate==false), are in the RUNNING stage, are healthy, and have no config error.
func (c *OmniClient) GetDriftingMachines(ctx context.Context, clusterName string) ([]MachineConfigDrift, error) {
	list, err := safe.StateListAll[*omnires.ClusterMachineStatus](ctx, c.state,
		state.WithLabelQuery(resource.LabelEqual(omnires.LabelCluster, clusterName)),
	)
	if err != nil {
		return nil, fmt.Errorf("list cluster machine statuses for %s: %w", clusterName, err)
	}

	var drifting []MachineConfigDrift

	list.ForEach(func(cms *omnires.ClusterMachineStatus) {
		spec := cms.TypedSpec().Value
		if !spec.ConfigUpToDate &&
			spec.Stage == omniSpecs.ClusterMachineStatusSpec_RUNNING &&
			spec.Ready &&
			spec.LastConfigError == "" {
			drifting = append(drifting, MachineConfigDrift{
				MachineID:         cms.Metadata().ID(),
				ManagementAddress: spec.ManagementAddress,
			})
		}
	})

	return drifting, nil
}

// RebootMachine issues a reboot to the specified machine via the Omni Talos API proxy.
// managementAddress is the machine's management address as reported by ClusterMachineStatus.
func (c *OmniClient) RebootMachine(ctx context.Context, clusterName, managementAddress string) error {
	talosClient := c.raw.Talos().WithCluster(clusterName).WithNodes(managementAddress)
	_, err := talosClient.Reboot(ctx, &machineapi.RebootRequest{})
	if err != nil {
		return fmt.Errorf("reboot machine at %s in cluster %s: %w", managementAddress, clusterName, err)
	}

	return nil
}

// ClusterStatus is the observed state returned from Omni.
type ClusterStatus struct {
	Phase string
	Ready bool // true when Omni reports kubernetesAPIReady
}

// EnsureCluster creates or updates the Omni Cluster resource.
func (c *OmniClient) EnsureCluster(ctx context.Context, name, kubeVersion, talosVersion string) error {
	md := resource.NewMetadata(omniresources.DefaultNamespace, omnires.ClusterType, name, resource.VersionUndefined)
	existing, err := safe.StateGet[*omnires.Cluster](ctx, c.state, md)

	if state.IsNotFoundError(err) {
		cluster := omnires.NewCluster(name)
		cluster.TypedSpec().Value.KubernetesVersion = kubeVersion
		cluster.TypedSpec().Value.TalosVersion = talosVersion
		return c.state.Create(ctx, cluster)
	}
	if err != nil {
		return fmt.Errorf("get cluster: %w", err)
	}

	if existing.TypedSpec().Value.KubernetesVersion != kubeVersion ||
		existing.TypedSpec().Value.TalosVersion != talosVersion {
		existing.TypedSpec().Value.KubernetesVersion = kubeVersion
		existing.TypedSpec().Value.TalosVersion = talosVersion
		return c.state.Update(ctx, existing)
	}
	return nil
}

// EnsureMachineSet creates the named MachineSet in Omni if it does not already exist.
// role is omnires.LabelControlPlaneRole or omnires.LabelWorkerRole.
func (c *OmniClient) EnsureMachineSet(ctx context.Context, clusterName, machineSetID, role string) error {
	md := resource.NewMetadata(omniresources.DefaultNamespace, omnires.MachineSetType, machineSetID, resource.VersionUndefined)
	_, err := safe.StateGet[*omnires.MachineSet](ctx, c.state, md)
	if err == nil {
		return nil
	}
	if !state.IsNotFoundError(err) {
		return fmt.Errorf("get machine set %s: %w", machineSetID, err)
	}
	ms := omnires.NewMachineSet(machineSetID)
	ms.Metadata().Labels().Set(omnires.LabelCluster, clusterName)
	ms.Metadata().Labels().Set(role, "")
	return c.state.Create(ctx, ms)
}

// DeleteMachineSet removes a MachineSet from Omni if it exists.
func (c *OmniClient) DeleteMachineSet(ctx context.Context, machineSetID string) error {
	md := resource.NewMetadata(omniresources.DefaultNamespace, omnires.MachineSetType, machineSetID, resource.VersionUndefined)
	if err := c.state.Destroy(ctx, md); err != nil && !state.IsNotFoundError(err) {
		return fmt.Errorf("delete machine set %s: %w", machineSetID, err)
	}
	return nil
}

// SelectAvailableMachines lists Omni MachineStatuses matching selector.matchLabels plus
// the built-in available constraint, and returns up to count machine UUIDs.
func (c *OmniClient) SelectAvailableMachines(ctx context.Context, sel metav1.LabelSelector, count int) ([]string, error) {
	labelOpts := []resource.LabelQueryOption{
		resource.LabelExists(omnires.MachineStatusLabelAvailable),
	}
	for k, v := range sel.MatchLabels {
		labelOpts = append(labelOpts, resource.LabelEqual(k, v))
	}

	list, err := safe.StateListAll[*omnires.MachineStatus](ctx, c.state,
		state.WithLabelQuery(labelOpts...),
	)
	if err != nil {
		return nil, fmt.Errorf("list machine statuses: %w", err)
	}

	var ids []string
	list.ForEach(func(ms *omnires.MachineStatus) {
		if len(ids) < count {
			ids = append(ids, ms.Metadata().ID())
		}
	})
	return ids, nil
}

// AllocatedMachineSetNodes lists machine UUIDs already bound to a MachineSet.
func (c *OmniClient) AllocatedMachineSetNodes(ctx context.Context, machineSetID string) ([]string, error) {
	list, err := safe.StateListAll[*omnires.MachineSetNode](ctx, c.state,
		state.WithLabelQuery(resource.LabelEqual(omnires.LabelMachineSet, machineSetID)),
	)
	if err != nil {
		return nil, fmt.Errorf("list machine set nodes for %s: %w", machineSetID, err)
	}

	var ids []string
	list.ForEach(func(n *omnires.MachineSetNode) {
		ids = append(ids, n.Metadata().ID())
	})
	return ids, nil
}

// EnsureMachineSetNode binds a machine to a MachineSet in Omni.
func (c *OmniClient) EnsureMachineSetNode(ctx context.Context, clusterName, machineSetID, machineID, role string) error {
	ms := omnires.NewMachineSet(machineSetID)
	ms.Metadata().Labels().Set(omnires.LabelCluster, clusterName)
	ms.Metadata().Labels().Set(role, "")

	node := omnires.NewMachineSetNode(machineID, ms)
	if err := c.state.Create(ctx, node); err != nil && !state.IsConflictError(err) {
		return fmt.Errorf("create machine set node %s in %s: %w", machineID, machineSetID, err)
	}
	return nil
}

// DeleteMachineSetNode removes a machine from its MachineSet binding in Omni.
// Omni's MachineSetStatusController holds a finalizer on MachineSetNode, so we
// must call Teardown first; if the resource isn't ready to destroy yet we return
// an error so controller-runtime requeues and retries on the next cycle.
func (c *OmniClient) DeleteMachineSetNode(ctx context.Context, machineID string) error {
	md := resource.NewMetadata(omniresources.DefaultNamespace, omnires.MachineSetNodeType, machineID, resource.VersionUndefined)

	ready, err := c.state.Teardown(ctx, md)
	if err != nil {
		if state.IsNotFoundError(err) {
			return nil
		}
		return fmt.Errorf("teardown machine set node %s: %w", machineID, err)
	}
	if !ready {
		return fmt.Errorf("machine set node %s teardown in progress, will retry", machineID)
	}

	if err := c.state.Destroy(ctx, md); err != nil && !state.IsNotFoundError(err) {
		return fmt.Errorf("delete machine set node %s: %w", machineID, err)
	}
	return nil
}

// EnsureConfigPatch creates or updates a Talos config patch for a machine in Omni.
func (c *OmniClient) EnsureConfigPatch(ctx context.Context, patchID, clusterName, machineSetID, machineID string, inline json.RawMessage) error {
	data, err := inline.MarshalJSON()
	if err != nil {
		return fmt.Errorf("marshal config patch: %w", err)
	}

	md := resource.NewMetadata(omniresources.DefaultNamespace, omnires.ConfigPatchType, patchID, resource.VersionUndefined)
	existing, getErr := safe.StateGet[*omnires.ConfigPatch](ctx, c.state, md)

	if state.IsNotFoundError(getErr) {
		p := omnires.NewConfigPatch(patchID,
			pair.MakePair(omnires.LabelCluster, clusterName),
			pair.MakePair(omnires.LabelMachineSet, machineSetID),
			pair.MakePair(omnires.LabelClusterMachine, machineID),
		)
		if err := p.TypedSpec().Value.SetUncompressedData(data); err != nil {
			return fmt.Errorf("set config patch data: %w", err)
		}
		return c.state.Create(ctx, p)
	}
	if getErr != nil {
		return fmt.Errorf("get config patch %s: %w", patchID, getErr)
	}

	buf, err := existing.TypedSpec().Value.GetUncompressedData()
	if err != nil {
		return fmt.Errorf("get existing config patch data: %w", err)
	}
	defer buf.Free()

	if string(buf.Data()) != string(data) {
		if err := existing.TypedSpec().Value.SetUncompressedData(data); err != nil {
			return fmt.Errorf("set config patch data: %w", err)
		}
		return c.state.Update(ctx, existing)
	}
	return nil
}

// EnsureMachineExtensions creates or updates the MachineExtensions resource for a machine.
// The ID of MachineExtensions.omni.sidero.dev is the machine UUID.
func (c *OmniClient) EnsureMachineExtensions(ctx context.Context, machineID string, extensions []string) error {
	md := resource.NewMetadata(omniresources.DefaultNamespace, omnires.MachineExtensionsType, machineID, resource.VersionUndefined)
	existing, err := safe.StateGet[*omnires.MachineExtensions](ctx, c.state, md)

	if state.IsNotFoundError(err) {
		me := omnires.NewMachineExtensions(machineID)
		me.TypedSpec().Value.Extensions = extensions
		return c.state.Create(ctx, me)
	}
	if err != nil {
		return fmt.Errorf("get machine extensions %s: %w", machineID, err)
	}

	current := existing.TypedSpec().Value.Extensions
	if !stringSlicesEqual(current, extensions) {
		existing.TypedSpec().Value.Extensions = extensions
		return c.state.Update(ctx, existing)
	}
	return nil
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// DeleteConfigPatchesForMachine removes all ConfigPatches scoped to a specific machine.
func (c *OmniClient) DeleteConfigPatchesForMachine(ctx context.Context, clusterName, machineID string) error {
	list, err := safe.StateListAll[*omnires.ConfigPatch](ctx, c.state,
		state.WithLabelQuery(
			resource.LabelEqual(omnires.LabelCluster, clusterName),
			resource.LabelEqual(omnires.LabelClusterMachine, machineID),
		),
	)
	if err != nil {
		return fmt.Errorf("list config patches for machine %s: %w", machineID, err)
	}

	var errs []error
	list.ForEach(func(p *omnires.ConfigPatch) {
		md := p.Metadata()
		if err := c.state.Destroy(ctx, md); err != nil && !state.IsNotFoundError(err) {
			errs = append(errs, fmt.Errorf("delete config patch %s: %w", md.ID(), err))
		}
	})

	if len(errs) > 0 {
		return fmt.Errorf("delete config patches for machine %s: %v", machineID, errs)
	}
	return nil
}

// GetClusterStatus returns the current Omni ClusterStatus for the named cluster.
func (c *OmniClient) GetClusterStatus(ctx context.Context, clusterName string) (ClusterStatus, error) {
	md := resource.NewMetadata(omniresources.DefaultNamespace, omnires.ClusterStatusType, clusterName, resource.VersionUndefined)
	cs, err := safe.StateGet[*omnires.ClusterStatus](ctx, c.state, md)
	if err != nil {
		if state.IsNotFoundError(err) {
			return ClusterStatus{Phase: "Unknown"}, nil
		}
		return ClusterStatus{}, fmt.Errorf("get cluster status: %w", err)
	}

	spec := cs.TypedSpec().Value
	return ClusterStatus{
		Phase: spec.Phase.String(),
		Ready: spec.KubernetesAPIReady,
	}, nil
}

// DeleteCluster removes the Omni Cluster resource.
func (c *OmniClient) DeleteCluster(ctx context.Context, clusterName string) error {
	md := resource.NewMetadata(omniresources.DefaultNamespace, omnires.ClusterType, clusterName, resource.VersionUndefined)
	if err := c.state.Destroy(ctx, md); err != nil && !state.IsNotFoundError(err) {
		return fmt.Errorf("delete cluster %s: %w", clusterName, err)
	}
	return nil
}

// GetKubeconfig fetches a service-account kubeconfig from Omni for the named cluster
// via the management API. The token TTL is 90 days; ensureKubeconfigSecret refreshes
// it on every reconcile so the actual expiry window is just a safety margin.
func (c *OmniClient) GetKubeconfig(ctx context.Context, clusterName string) ([]byte, error) {
	data, err := c.mgmt.WithCluster(clusterName).Kubeconfig(ctx,
		management.WithServiceAccount(90*24*time.Hour, "flux-admin", "system:masters"),
	)
	if err != nil {
		return nil, fmt.Errorf("get kubeconfig for %s: %w", clusterName, err)
	}
	return data, nil
}
