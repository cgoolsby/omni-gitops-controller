# Prompt 05 — Fix `matchExpressions` Silently Ignored in Machine Selection

## Context

`MachineSetSpec.MachineSelector` is declared as `metav1.LabelSelector`, which supports both:
- `matchLabels`: key/value equality constraints
- `matchExpressions`: richer constraints (`In`, `NotIn`, `Exists`, `DoesNotExist`)

In `SelectAvailableMachines` (`controllers/omni_client.go:146`), only `matchLabels` is processed:

```go
for k, v := range sel.MatchLabels {
    labelOpts = append(labelOpts, resource.LabelEqual(k, v))
}
```

`sel.MatchExpressions` is never read. If a user specifies:

```yaml
machineSelector:
  matchExpressions:
    - key: omni.sidero.dev/platform
      operator: NotIn
      values: ["aws", "gcp"]
```

...the constraint is silently dropped and any available machine is a candidate, including AWS/GCP machines the user explicitly wanted to exclude. There is no error or warning.

---

## What to Do

### Option A: Implement `matchExpressions` support

The Omni COSI label query API (`resource.LabelQueryOption`) supports:
- `resource.LabelEqual(k, v)` — for `In` with a single value / `matchLabels`
- `resource.LabelExists(k)` — for `Exists`
- `resource.LabelNotExists(k)` — for `DoesNotExist` (check if this exists in the cosi-project/runtime API)

For `In` / `NotIn` with multiple values, check the COSI runtime API for `LabelIn` / `LabelNotIn` functions. If they exist, implement full translation:

```go
for _, expr := range sel.MatchExpressions {
    switch expr.Operator {
    case metav1.LabelSelectorOpIn:
        // translate to label query — may need LabelIn or multiple LabelEqual OR'd
    case metav1.LabelSelectorOpNotIn:
        // translate to LabelNotIn or filter post-list
    case metav1.LabelSelectorOpExists:
        labelOpts = append(labelOpts, resource.LabelExists(expr.Key))
    case metav1.LabelSelectorOpDoesNotExist:
        labelOpts = append(labelOpts, resource.LabelNotExists(expr.Key))
    }
}
```

If the COSI API doesn't support `NotIn` natively, implement it as a post-list filter.

### Option B: Validate and reject unsupported expressions (minimum viable fix)

If full implementation is out of scope, add a validation guard at the top of `SelectAvailableMachines` (or in the reconciler before calling it) that returns an error when `matchExpressions` is non-empty:

```go
if len(sel.MatchExpressions) > 0 {
    return nil, fmt.Errorf("machineSelector.matchExpressions is not yet supported; use matchLabels only")
}
```

This converts a silent correctness bug into a visible error. Document the limitation in the README.

---

## Verification

- Write a test in `omnicluster_controller_test.go` that pre-creates machines with a label and queries using a `DoesNotExist` or `NotIn` expression, then verifies the correct machines are excluded (Option A) or that an error is returned (Option B).
- Confirm existing tests still pass.
