# Prompt 10 — Don't Silently Claim Machines Bound to Another Machine Set

## Context

`EnsureMachineSetNode` (`controllers/omni_client.go`) swallows conflict errors:

```go
if err := c.state.Create(ctx, node); err != nil && !state.IsConflictError(err) {
    return ...
}
```

If the machine was bound to a **different** machine set between
`SelectAvailableMachines` and `Create` (another OmniCluster, a human in the Omni
UI, or a future bump of `MaxConcurrentReconciles`), the conflict is ignored and
the controller counts the machine as allocated to *this* set — the allocation
silently diverges from reality.

## What to Do

In `EnsureMachineSetNode`, when `Create` returns a conflict error:

1. Fetch the existing `MachineSetNode` (ID = machine UUID).
2. Read its `omnires.LabelMachineSet` label.
3. If it matches `machineSetID` → the binding already exists as desired; return
   nil (today's intended behaviour, now verified).
4. If it differs or the label is missing → return an error naming both machine
   sets, so the reconciler surfaces the conflict via `setFailure` and retries
   with a different machine on the next cycle.

Check how Omni sets the machine-set label on `MachineSetNode` — the
`NewMachineSetNode(machineID, ms)` constructor derives labels from the owning
`MachineSet`; inspect the vendored constructor to confirm the label key used and
match it in the verification.

## Tests

In `controllers/omnicluster_controller_test.go`:

- Bind machine `m1` to set `cluster-a-workers` via `EnsureMachineSetNode`; call
  `EnsureMachineSetNode` again for the **same** set → nil error (idempotent).
- Call `EnsureMachineSetNode` for `m1` with a **different** set ID → error
  mentioning the conflicting set.

## Constraints

- Do NOT commit. Do NOT touch `metaPrompts/` or unrelated files.

## Acceptance Criteria

- `gofmt -l .` clean; `go vet ./...`, `go build ./...`, `go test ./...` pass.
