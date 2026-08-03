package controllers

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestClusterMetrics_SetAndDelete(t *testing.T) {
	name, namespace := "metrics-test", "default"

	clusterReadyGauge.WithLabelValues(name, namespace).Set(1)
	clusterConfigDriftGauge.WithLabelValues(name, namespace).Set(0)
	machineRebootsTotal.WithLabelValues(name, namespace).Inc()
	clusterMachinesAllocatedGauge.WithLabelValues(name, namespace, name+"-control-planes").Set(3)
	clusterMachinesAllocatedGauge.WithLabelValues(name, namespace, name+"-workers").Set(2)

	if got := testutil.ToFloat64(clusterReadyGauge.WithLabelValues(name, namespace)); got != 1 {
		t.Errorf("ready gauge = %v, want 1", got)
	}

	clusterReadyGauge.WithLabelValues(name, namespace).Set(0)
	if got := testutil.ToFloat64(clusterReadyGauge.WithLabelValues(name, namespace)); got != 0 {
		t.Errorf("ready gauge after Set(0) = %v, want 0", got)
	}

	deleteClusterMetrics(name, namespace)

	if got := testutil.CollectAndCount(clusterMachinesAllocatedGauge); got != 0 {
		t.Errorf("machines allocated series after delete = %d, want 0", got)
	}
	if got := testutil.CollectAndCount(clusterReadyGauge); got != 0 {
		t.Errorf("ready series after delete = %d, want 0", got)
	}
	if got := testutil.CollectAndCount(machineRebootsTotal); got != 0 {
		t.Errorf("reboot counter series after delete = %d, want 0", got)
	}
}
