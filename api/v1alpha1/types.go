package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ── OmniCluster ───────────────────────────────────────────────────────────────

// OmniClusterSpec is the desired state of a cluster managed by Omni.
// +kubebuilder:validation:XValidation:rule="self.controlPlane.replicas >= 1",message="controlPlane.replicas must be at least 1"
type OmniClusterSpec struct {
	// KubernetesVersion is the target Kubernetes version (e.g. "1.31.0").
	// +kubebuilder:validation:Pattern=`^\d+\.\d+\.\d+$`
	KubernetesVersion string `json:"kubernetesVersion"`
	// TalosVersion is the target Talos Linux version (e.g. "v1.13.0").
	// +kubebuilder:validation:Pattern=`^v\d+\.\d+\.\d+(-[A-Za-z0-9.]+)?$`
	TalosVersion string `json:"talosVersion"`
	// ControlPlane describes the control-plane machine set.
	ControlPlane MachineSetSpec `json:"controlPlane"`
	// Workers describes additional worker machine sets. Empty for single-node clusters.
	// +optional
	Workers []WorkerMachineSetSpec `json:"workers,omitempty"`
}

// MachineSetSpec describes a set of machines within a cluster (control plane or workers).
type MachineSetSpec struct {
	// Replicas is the desired number of machines in this set.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas,omitempty"`
	// MachineSelector selects available Omni machines by label.
	// Both matchLabels and matchExpressions are supported, with standard
	// Kubernetes label-selector semantics. Matched against
	// MachineStatuses.omni.sidero.dev labels (e.g. omni.sidero.dev/mem,
	// omni.sidero.dev/cpu, omni.sidero.dev/platform).
	// The controller always adds omni.sidero.dev/available to the query.
	MachineSelector metav1.LabelSelector `json:"machineSelector"`
	// MachineExtensions is the list of Talos system extensions to install on each machine in the set.
	// When non-empty, the controller creates or updates the MachineExtensions.omni.sidero.dev resource
	// for each matched machine. When absent or empty, existing MachineExtensions are left untouched.
	// +optional
	MachineExtensions []string `json:"machineExtensions,omitempty"`
	// KernelArgs is the list of extra kernel arguments to include in the machine schematic.
	// These are applied at the schematic level so they are active during maintenance/install boot,
	// not just on the installed system. Use for args needed before first install (e.g. libata.force=noncq).
	// +optional
	KernelArgs []string `json:"kernelArgs,omitempty"`
	// ConfigPatches are Talos machine config patches applied to each machine in the set.
	// +optional
	ConfigPatches []ConfigPatch `json:"configPatches,omitempty"`
}

// WorkerMachineSetSpec is a named worker machine set.
type WorkerMachineSetSpec struct {
	// Name identifies this worker set. Used as the Omni MachineSet suffix
	// (resulting ID: <cluster>-<name>).
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name           string `json:"name"`
	MachineSetSpec `json:",inline"`
}

// ConfigPatch is a Talos machine config patch applied via Omni's ConfigPatch resource.
type ConfigPatch struct {
	// Name is a unique identifier for this patch within the machine set.
	Name string `json:"name"`
	// Inline is the patch content in Talos machine config YAML/JSON format.
	Inline apiextensionsv1.JSON `json:"inline,omitempty"`
}

// OmniClusterStatus is the observed state of the cluster.
type OmniClusterStatus struct {
	// Phase mirrors the Omni ClusterStatus phase (ScalingUp, Running, Destroying, etc.).
	// +optional
	Phase string `json:"phase,omitempty"`
	// Ready is true when the Omni cluster is Running and the Kubernetes API is reachable.
	Ready bool `json:"ready,omitempty"`
	// AllocatedMachines records the Omni machine UUIDs bound to this cluster,
	// keyed by machine-set ID. Used to detect drift on re-reconcile.
	// +optional
	AllocatedMachines map[string][]string `json:"allocatedMachines,omitempty"`
	// LastRebootTimes records when the controller last issued a drift-remediation
	// reboot for each machine (keyed by machine UUID). Used to enforce a cooldown
	// so a persistently-drifting machine is not rebooted in a loop.
	// +optional
	LastRebootTimes map[string]metav1.Time `json:"lastRebootTimes,omitempty"`
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
// +kubebuilder:printcolumn:name="K8s",type=string,JSONPath=`.spec.kubernetesVersion`
// +kubebuilder:printcolumn:name="Talos",type=string,JSONPath=`.spec.talosVersion`
// +kubebuilder:printcolumn:name="CP",type=integer,JSONPath=`.spec.controlPlane.replicas`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// OmniCluster declares the desired state of a Talos Kubernetes cluster managed by Omni.
// The controller reconciles this resource into the full set of Omni resources:
// Cluster, MachineSet(s), MachineSetNode(s), and ConfigPatch(es).
type OmniCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              OmniClusterSpec   `json:"spec,omitempty"`
	Status            OmniClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OmniClusterList contains a list of OmniCluster.
type OmniClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OmniCluster `json:"items"`
}
