# Prompt 07 — Evicted Machines Must Release Their KernelArgs

## Context

In the scale-down branch of `reconcileMachineSet`
(`controllers/omnicluster_controller.go`), each evicted machine gets
`DeleteMachineSetNode` and `DeleteConfigPatchesForMachine` — but **not**
`DeleteKernelArgsForMachine`. The per-machine `KernelArgs` resource (keyed by
machine UUID, no cluster label) survives the eviction, so the machine returns to
Omni's available pool still carrying custom kernel args (e.g.
`libata.force=noncq`). The next cluster that allocates that machine silently
inherits them.

(Prompt 06 fixed the same leak on full cluster deletion; this prompt fixes the
scale-down path.)

## What to Do

In the eviction loop of `reconcileMachineSet`, after
`DeleteConfigPatchesForMachine`, call
`r.OmniClient.DeleteKernelArgsForMachine(ctx, machineID)` and wrap the error with
context like the neighbouring calls.

## Tests

Add a test in `controllers/omnicluster_controller_test.go` modelled on
`TestReconcileMachineSet_PrunesKernelArgsWhenEmpty`:

1. Pre-create a worker `MachineSetNode` (use `setupAllocatedMachine`).
2. Reconcile with `Replicas: 1` and `KernelArgs: []string{"libata.force=noncq"}`
   → assert the `KernelArgs` resource exists.
3. Reconcile again with `Replicas: 0` (worker role; still passing the same
   `KernelArgs` in the spec, so the deletion must come from the eviction path,
   not the prune-on-clear path).
4. Assert the machine's `KernelArgs` resource is gone and the `MachineSetNode`
   is gone.

## Constraints

- Do NOT commit. Do NOT touch `metaPrompts/` or unrelated files.

## Acceptance Criteria

- `gofmt -l .` clean; `go vet ./...`, `go build ./...`, `go test ./...` pass.
