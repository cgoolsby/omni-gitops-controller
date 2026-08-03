# Prompt 10 — Add Custom Prometheus Metrics

## Context

The controller exposes controller-runtime's default metrics on `:8080` but has
no project-specific metrics. For production alerting we want to answer: is any
cluster stuck non-Ready? how many machines per machine set? is drift detected?
how many reboots have been triggered?

## What to Do

### 1. Define metrics in a new `controllers/metrics.go`

Register on `sigs.k8s.io/controller-runtime/pkg/metrics`.Registry in an
`init()`:

- `omni_cluster_ready` GaugeVec `{name, namespace}` — 1/0
- `omni_cluster_machines_allocated` GaugeVec `{name, namespace, machineset}`
- `omni_cluster_config_drift_detected` GaugeVec `{name, namespace}` — 1/0
- `omni_machine_reboots_total` CounterVec `{name, namespace}`

(`prometheus/client_golang` is already an indirect dependency via
controller-runtime; promote it in go.mod as needed with `go mod tidy`.)

### 2. Update metrics in the reconcile loop

In `controllers/omnicluster_controller.go`:

- Set `omni_cluster_ready` after the Omni status is fetched.
- Set `omni_cluster_machines_allocated` per machine-set from the
  `allocatedMachines` map.
- Set `omni_cluster_config_drift_detected` after the drift check (0 when the
  cluster isn't ready / no drift).
- Increment `omni_machine_reboots_total` where the reboot is issued.

### 3. Clean up on deletion

In the delete path (after `deleteOmniResources` succeeds), remove the cluster's
series to avoid stale metrics: `DeleteLabelValues(name, namespace)` for the
plain gauges, and `DeletePartialMatch(prometheus.Labels{"name": ..., "namespace": ...})`
for the per-machineset gauge.

## Tests

Existing tests must pass (metric updates on a nil-safe global registry are fine
from unit tests). No dedicated metrics test required; if straightforward, assert
via `prometheus/client_golang/prometheus/testutil.ToFloat64` that the ready
gauge changes — optional.

## Constraints

- Do NOT commit. Do NOT touch `metaPrompts/` or unrelated files.

## Acceptance Criteria

- `gofmt -l .` clean; `go vet ./...`, `go build ./...`, `go test ./...` pass.
- `go mod tidy` leaves go.mod/go.sum consistent.
