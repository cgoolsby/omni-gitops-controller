# Prompt 07 — Implement `matchExpressions` in Machine Selection

## Context

`MachineSetSpec.MachineSelector` is a `metav1.LabelSelector`, but
`SelectAvailableMachines` (`controllers/omni_client.go`) only translates
`matchLabels` into the Omni COSI label query. `matchExpressions` used to be
silently ignored; it is currently **rejected at admission** by a CEL rule on
`MachineSetSpec` in `api/v1alpha1/types.go` (look for the
`machineSelector.matchExpressions is not supported yet` XValidation marker, with
a comment saying to remove it once support lands). This prompt lands that
support.

## What to Do

### 1. Implement selection

In `SelectAvailableMachines`, support all four operators (`In`, `NotIn`,
`Exists`, `DoesNotExist`) with standard Kubernetes label-selector semantics
(e.g. a machine *lacking* the key matches `NotIn` and `DoesNotExist`).

Recommended approach — server-side narrowing plus client-side exactness:

- Keep the existing COSI query: `LabelExists(MachineStatusLabelAvailable)` plus
  `LabelEqual` for each `matchLabels` entry. Inspect the vendored
  `cosi-project/runtime` label-query options (`resource.Label*` in
  `pkg/resource`) and push down whatever operators it supports natively
  (`LabelExists` certainly; check for `LabelIn`/negation support).
- For exact semantics, convert the full selector once via
  `metav1.LabelSelectorAsSelector(&sel)` (`k8s.io/apimachinery/pkg/apis/meta/v1`
  + `k8s.io/apimachinery/pkg/labels`) and, in the `ForEach`, build a
  `labels.Set` from the machine's metadata labels and keep the machine only if
  `selector.Matches(set)`. This guarantees correct `NotIn`/`DoesNotExist`
  behaviour regardless of what COSI can push down.
- Return an error for an invalid selector (`LabelSelectorAsSelector` error).

### 2. Remove the admission rejection

Delete the CEL `XValidation` rule rejecting `matchExpressions` from
`api/v1alpha1/types.go` (and its accompanying comment), then regenerate:

```bash
make manifests generate   # Makefile from prompt 03; falls back to controller-gen directly
```

Both `config/crd/bases/` and `charts/omni-gitops-controller/crds/` must be
regenerated/copied.

### 3. README

If the README documents the machineSelector field, mention that both
`matchLabels` and `matchExpressions` are supported.

## Tests

In `controllers/omnicluster_controller_test.go`, pre-create `MachineStatus`
resources in the in-memory state with the `MachineStatusLabelAvailable` label
plus distinguishing labels, then verify:

- `Exists` selects only machines with the key.
- `DoesNotExist` excludes machines with the key.
- `In` with two values selects exactly the matching machines.
- `NotIn` excludes listed values but **includes machines lacking the key**.
- `matchLabels` and `matchExpressions` combined AND together.

## Constraints

- Do NOT commit. Do NOT touch `metaPrompts/` or unrelated files.

## Acceptance Criteria

- `gofmt -l .` clean; `go vet ./...`, `go build ./...`, `go test ./...` pass.
- The CEL rejection rule is gone from types.go and both CRD copies.
