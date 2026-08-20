# Prompt 06 — Prune Orphaned Worker Machine Sets During Reconcile

## Context

If a user removes a worker set from `spec.workers` (without deleting the entire `OmniCluster`), the reconciler does **not** clean up the corresponding Omni `MachineSet` and its `MachineSetNode` resources. The machines stay bound, consuming capacity that could otherwise be allocated elsewhere.

The current reconcile loop (`Reconcile` in `controllers/omnicluster_controller.go`) iterates `cluster.Spec.Workers` and calls `EnsureMachineSet` + `reconcileMachineSet` for each declared worker set. It never compares the desired worker sets against what was previously allocated.

The `status.AllocatedMachines` map tracks all currently allocated machine sets keyed by machine-set ID. This is the right data source for detecting orphaned sets.

---

## What to Do

After the worker reconcile loop (after all desired worker sets have been reconciled), add a pruning step that:

1. Builds a set of desired machine-set IDs from `cluster.Spec.Workers`:
   ```go
   desired := map[string]bool{
       cpMachineSetID: true,
   }
   for _, w := range cluster.Spec.Workers {
       desired[fmt.Sprintf("%s-%s", clusterName, w.Name)] = true
   }
   ```

2. Iterates `cluster.Status.AllocatedMachines` and for any key not in `desired`:
   - Calls `r.OmniClient.DeleteMachineSetNode` for each machine in the set
   - Calls `r.OmniClient.DeleteMachineSet` for the machine set itself
   - Calls `r.OmniClient.DeleteConfigPatchesForMachine` for each evicted machine
   - Calls `r.OmniClient.DeleteExtensionsConfigurationForMachineSet` for the machine set
   - Removes the key from `allocatedMachines` (the local variable being built for the status update)

3. The pruning errors should propagate as a reconcile failure with reason `PruneOrphanedMachineSetFailed`.

Place this step **before** the status update so the updated `allocatedMachines` map correctly reflects the post-prune state.

---

## Verification

1. Write a test in `omnicluster_controller_test.go`:
   - Pre-create an `OmniCluster` status with a worker set `test-cluster-old-workers` in `AllocatedMachines`
   - Set `spec.workers` to an empty list (or a different worker set name)
   - Run `reconcileMachineSet` or mock the full reconcile path
   - Assert that the `MachineSet` for `test-cluster-old-workers` is gone from the in-memory state
2. Manually test: apply an `OmniCluster` with a worker set, wait for allocation, remove the worker set from the spec, apply the update, and verify the machines are released in Omni.
