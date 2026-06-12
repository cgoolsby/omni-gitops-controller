# Prompt 06 — Prune Orphaned Worker Machine Sets During Reconcile

## Context

If a user removes a worker set from `spec.workers` (without deleting the whole
`OmniCluster`), the reconciler never cleans up the corresponding Omni
`MachineSet` and its `MachineSetNode`s — the machines stay bound forever,
consuming capacity. The reconcile loop in
`controllers/omnicluster_controller.go` iterates `cluster.Spec.Workers` and
ensures each declared set, but never compares desired sets against what exists
in Omni.

Important design constraint: **Omni state is the source of truth**, not
`status.AllocatedMachines` (the teardown path was already converted to this
model — see `deleteOmniResources`, which uses
`ListMachineSetIDsForCluster` from `controllers/omni_client.go`). The pruning
step must use the same approach.

## What to Do

In `Reconcile`, after the worker reconcile loop and before the status update:

1. Build the desired set of machine-set IDs:
   `omnires.ControlPlanesResourceID(clusterName)` plus
   `fmt.Sprintf("%s-%s", clusterName, w.Name)` for each declared worker.
2. List actual sets with the existing
   `r.OmniClient.ListMachineSetIDsForCluster(ctx, clusterName)`.
3. For each actual set NOT in desired:
   - **Never prune the control-plane set** (it is always in desired; assert this
     defensively anyway).
   - For each machine in the set (`AllocatedMachineSetNodes`):
     `DeleteMachineSetNode`, `DeleteConfigPatchesForMachine`,
     `DeleteKernelArgsForMachine` — same cleanup trio as the eviction path in
     `reconcileMachineSet`.
   - `DeleteExtensionsConfigurationForMachineSet`, then `DeleteMachineSet`.
4. On error, fail the reconcile via the existing `setFailure` mechanism with
   reason `PruneOrphanedMachineSetFailed`.
5. Make sure the pruned sets are absent from the `allocatedMachines` map used
   for the status update (they will be, since the map is built from
   `spec`-declared sets — verify and add a comment).

Note: `DeleteMachineSetNode` returns an error while Omni teardown is in
progress; that error propagating through `setFailure` gives the retry/requeue
behaviour we want. Extract the per-machine cleanup trio into a small helper if
it avoids triplicating code between eviction, pruning, and teardown.

## Tests

In `controllers/omnicluster_controller_test.go`, using the in-memory state
helpers:

- Create two worker machine sets for a cluster (via `EnsureMachineSet` +
  `setupAllocatedMachine`), with a `KernelArgs` and a `ConfigPatch` per machine
  and an `ExtensionsConfiguration` per set.
- Run the pruning logic with a desired-worker list containing only one of them.
- Assert the orphaned set, its nodes, patches, kernel args, and extensions
  configuration are gone, and the surviving set is untouched.
- Assert the control-plane set is never selected for pruning.

Structure the pruning logic as a method that can be called directly from tests
(like `reconcileMachineSet`), rather than only being reachable through
`Reconcile`.

## Constraints

- Do NOT commit. Do NOT touch `metaPrompts/` or unrelated files.

## Acceptance Criteria

- `gofmt -l .` clean; `go vet ./...`, `go build ./...`, `go test ./...` pass.
- Removing a worker set from the spec releases all its Omni resources.
