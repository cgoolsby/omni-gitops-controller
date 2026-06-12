# Prompt 08 — Add Top-Level `status.observedGeneration`

## Context

The controller already sets `ObservedGeneration` on individual **conditions**
(`meta.SetStatusCondition` calls in `controllers/omnicluster_controller.go`),
but `OmniClusterStatus` has no top-level `observedGeneration` field. Tooling
(Flux `dependsOn`, `kubectl wait`, kstatus) uses the top-level field to decide
whether status is fresh relative to the spec; without it, `status.ready: true`
may reflect a previous spec generation.

## What to Do

### 1. Add the field

In `api/v1alpha1/types.go`, add to `OmniClusterStatus`:

```go
// ObservedGeneration is the .metadata.generation last processed by the controller.
// It is set on both success and failure paths: the controller observed the
// generation either way, and conditions carry the success/failure signal.
// +optional
ObservedGeneration int64 `json:"observedGeneration,omitempty"`
```

### 2. Set it in the reconciler

In `controllers/omnicluster_controller.go`, set
`cluster.Status.ObservedGeneration = cluster.Generation` on **both** the success
path and in `setFailure` (before each status update). Make sure the
conditional-update logic (`statusBefore` comparison) still works — the field is
part of status, so a generation bump alone correctly triggers one status write.

### 3. Regenerate

```bash
make generate manifests
```

(regenerates `zz_generated.deepcopy.go`, `config/crd/bases/`, and the chart's
`crds/` copy).

## Tests

Existing tests must pass. If `setFailure` or the status-update decision has any
directly testable helper, extend its test to cover the new field; otherwise no
new test is required for a plain field assignment.

## Constraints

- Do NOT commit. Do NOT touch `metaPrompts/` or unrelated files.

## Acceptance Criteria

- `gofmt -l .` clean; `go vet ./...`, `go build ./...`, `go test ./...` pass.
- Both CRD copies regenerated and identical.
