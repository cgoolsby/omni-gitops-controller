package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ── Machine ────────────────────────────────────────────────────────────────────

// MachineSpec is the desired per-machine Omni state. A Machine is created,
// updated and deleted by the OmniClusterReconciler to express "this cluster
// needs this machine bound to this set with this config" — the
// Deployment→ReplicaSet→Pod / Cluster-API Cluster→MachineSet→Machine shape.
// The MachineReconciler converges a single machine's Omni state onto this spec.
//
// metadata.name of a Machine is the raw Omni machine UUID: globally unique and
// matching Omni's own identity, so there is no invented naming scheme to map.
type MachineSpec struct {
	// ClusterName is the Omni cluster this machine belongs to.
	ClusterName string `json:"clusterName"`
	// MachineSetID is the Omni-side machine-set ID this machine is bound to
	// (e.g. "<cluster>-control-planes" or "<cluster>-<worker>").
	MachineSetID string `json:"machineSetID"`
	// Role is the machine's role within the set.
	// +kubebuilder:validation:Enum=control-plane;worker
	Role string `json:"role"`
	// ConfigPatches are the Talos machine config patches applied to this machine.
	// +optional
	ConfigPatches []ConfigPatch `json:"configPatches,omitempty"`
	// KernelArgs are extra kernel arguments applied to this machine at the
	// schematic level (active during maintenance/install boot).
	// +optional
	KernelArgs []string `json:"kernelArgs,omitempty"`
	// RebootRequestedAt requests a drift-remediation reboot of this machine.
	// A reboot is "owed" iff RebootRequestedAt is newer than
	// Status.LastRebootTime; nil means no reboot is owed. The requester never
	// has to clear this field — stamping Status.LastRebootTime after the reboot
	// RPC runs naturally makes the reboot no longer owed.
	// +optional
	RebootRequestedAt *metav1.Time `json:"rebootRequestedAt,omitempty"`
	// ManagementAddress is the machine's management address, meaningful only
	// alongside a RebootRequestedAt — it is the address the reboot RPC targets.
	// +optional
	ManagementAddress string `json:"managementAddress,omitempty"`
}

// MachineStatus is the observed per-machine state.
type MachineStatus struct {
	// ObservedGeneration is the .metadata.generation last processed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Ready is true when the machine is bound and its config is applied.
	// +optional
	Ready bool `json:"ready,omitempty"`
	// Phase is a short lifecycle token (Binding, Ready, Rebooting, Failed).
	// +optional
	Phase string `json:"phase,omitempty"`
	// LastRebootTime records when the reboot RPC last ran for this machine.
	// It is stamped only after the RPC returns, so it is never persisted without
	// a matching reboot attempt. Used both to make a requested reboot no longer
	// owed and to enforce the cross-machine reboot cooldown.
	// +optional
	LastRebootTime *metav1.Time `json:"lastRebootTime,omitempty"`
	// FailureReason is a short machine-readable token when phase is Failed.
	// +optional
	FailureReason *string `json:"failureReason,omitempty"`
	// FailureMessage is a human-readable error description.
	// +optional
	FailureMessage *string `json:"failureMessage,omitempty"`
	// Conditions is the standard Kubernetes condition set.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterName`
// +kubebuilder:printcolumn:name="Set",type=string,JSONPath=`.spec.machineSetID`
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.spec.role`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// Machine owns all per-machine Omni state for one machine bound to a cluster:
// its bind/unbind to the Omni machine set, its config patches and kernel args,
// and its drift-remediation reboots. It is reconciled by the MachineReconciler
// and owned (controller reference) by the OmniCluster that created it.
type Machine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MachineSpec   `json:"spec,omitempty"`
	Status            MachineStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MachineList contains a list of Machine.
type MachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Machine `json:"items"`
}
