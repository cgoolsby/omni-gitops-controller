# Prompt 09 — Add Custom Prometheus Metrics

## Context

The controller exposes a metrics endpoint at `:8080` via `controller-runtime`'s default metrics server, which publishes standard controller-runtime metrics (reconcile queue depth, work duration, etc.). However, there are no custom metrics specific to `omni-gitops-controller`.

These would be high value for production monitoring and alerting:

- **Is any cluster stuck in a non-Ready state?**
- **How many machines are allocated per cluster?**
- **Is config drift detected on any cluster?**
- **How many machine reboots have been triggered?**

---

## What to Do

### 1. Define custom metrics using `prometheus/client_golang`

The project already transitively depends on `prometheus/client_golang` via `controller-runtime`. Register custom metrics in a new file `controllers/metrics.go`:

```go
package controllers

import (
    "github.com/prometheus/client_golang/prometheus"
    "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
    clusterReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
        Name: "omni_cluster_ready",
        Help: "1 if the OmniCluster is Ready, 0 otherwise.",
    }, []string{"name", "namespace"})

    machinesAllocated = prometheus.NewGaugeVec(prometheus.GaugeOpts{
        Name: "omni_cluster_machines_allocated",
        Help: "Number of machines currently allocated to a machine set.",
    }, []string{"name", "namespace", "machineset"})

    configDriftDetected = prometheus.NewGaugeVec(prometheus.GaugeOpts{
        Name: "omni_cluster_config_drift_detected",
        Help: "1 if config drift is detected on the OmniCluster, 0 otherwise.",
    }, []string{"name", "namespace"})

    machineRebootsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
        Name: "omni_machine_reboots_total",
        Help: "Total number of machine reboots triggered for config drift resolution.",
    }, []string{"name", "namespace"})
)

func init() {
    metrics.Registry.MustRegister(
        clusterReady,
        machinesAllocated,
        configDriftDetected,
        machineRebootsTotal,
    )
}
```

### 2. Update metrics in the reconcile loop

In `omnicluster_controller.go`:

- After `cluster.Status.Ready = omniStatus.Ready`:
  ```go
  readyVal := 0.0
  if omniStatus.Ready { readyVal = 1.0 }
  clusterReady.WithLabelValues(cluster.Name, cluster.Namespace).Set(readyVal)
  ```

- After building `allocatedMachines`:
  ```go
  for msID, machines := range allocatedMachines {
      machinesAllocated.WithLabelValues(cluster.Name, cluster.Namespace, msID).Set(float64(len(machines)))
  }
  ```

- After drift check:
  ```go
  driftVal := 0.0
  if len(drifting) > 0 { driftVal = 1.0 }
  configDriftDetected.WithLabelValues(cluster.Name, cluster.Namespace).Set(driftVal)
  ```

- When triggering a reboot:
  ```go
  machineRebootsTotal.WithLabelValues(cluster.Name, cluster.Namespace).Inc()
  ```

### 3. Clean up metrics on cluster deletion

In `deleteOmniResources` (or at the end of the delete path), call `DeleteLabelValues` to avoid stale metric series after a cluster is removed:

```go
clusterReady.DeleteLabelValues(cluster.Name, cluster.Namespace)
configDriftDetected.DeleteLabelValues(cluster.Name, cluster.Namespace)
// machinesAllocated needs to delete per-machineset label combos
```

### 4. Add a `ServiceMonitor` to the Helm chart (optional, see prompt 14)

---

## Verification

```bash
kubectl port-forward -n omni-gitops-system deploy/omni-gitops-controller 8080:8080
curl -s http://localhost:8080/metrics | grep omni_
# Should show omni_cluster_ready, omni_cluster_machines_allocated, etc.
```
