package controllers

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	clusterReadyGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "omni_cluster_ready",
			Help: "Whether the Omni cluster is ready (1) or not (0).",
		},
		[]string{"name", "namespace"},
	)
	clusterMachinesAllocatedGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "omni_cluster_machines_allocated",
			Help: "Number of machines allocated to each machine set of the cluster.",
		},
		[]string{"name", "namespace", "machineset"},
	)
	clusterConfigDriftGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "omni_cluster_config_drift_detected",
			Help: "Whether config drift is detected on any machine of the cluster (1) or not (0).",
		},
		[]string{"name", "namespace"},
	)
	machineRebootsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "omni_machine_reboots_total",
			Help: "Total number of drift-remediation machine reboots triggered for the cluster.",
		},
		[]string{"name", "namespace"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		clusterReadyGauge,
		clusterMachinesAllocatedGauge,
		clusterConfigDriftGauge,
		machineRebootsTotal,
	)
}

// deleteClusterMetrics removes all metric series for a cluster so deleted
// clusters do not linger as stale series on the /metrics endpoint.
func deleteClusterMetrics(name, namespace string) {
	clusterReadyGauge.DeleteLabelValues(name, namespace)
	clusterConfigDriftGauge.DeleteLabelValues(name, namespace)
	machineRebootsTotal.DeleteLabelValues(name, namespace)
	clusterMachinesAllocatedGauge.DeletePartialMatch(prometheus.Labels{
		"name":      name,
		"namespace": namespace,
	})
}

func boolToFloat64(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
