# Prompt 06 — Finalizer Teardown Driven by Omni State, Not status.AllocatedMachines

## Context

`deleteOmniResources` (`controllers/omnicluster_controller.go`) iterates
`cluster.Status.AllocatedMachines` to find what to tear down. That map can be
**empty or stale**: a CR deleted before its first successful reconcile, or after a
partially-failed reconcile, leaks `MachineSetNode`s and `MachineSet`s in Omni —
exactly the orphaned-machines problem the finalizer exists to prevent. The README
states Omni is authoritative for state; teardown should honour that.

Teardown also leaves behind per-set `ExtensionsConfiguration` resources and
per-machine `KernelArgs` resources (machines return to the available pool still
carrying custom kernel args — the next cluster that allocates them inherits them).

## What to Do

### 1. New client method

In `controllers/omni_client.go` add:

```go
// ListMachineSetIDsForCluster returns the IDs of all Omni MachineSets labelled
// with the given cluster.
func (c *OmniClient) ListMachineSetIDsForCluster(ctx context.Context, clusterName string) ([]string, error)
```

implemented with `safe.StateListAll[*omnires.MachineSet]` and a
`resource.LabelEqual(omnires.LabelCluster, clusterName)` label query, mirroring
`AllocatedMachineSetNodes`.

### 2. Rewrite deleteOmniResources

Drive teardown entirely from Omni state:

1. `sets := ListMachineSetIDsForCluster(clusterName)`.
2. For each set: `nodes := AllocatedMachineSetNodes(setID)`; for each node:
   - `DeleteMachineSetNode(machineID)` (existing teardown-in-progress error
     semantics are kept — returning an error makes controller-runtime requeue,
     which is the desired retry behaviour),
   - `DeleteConfigPatchesForMachine(clusterName, machineID)`,
   - `DeleteKernelArgsForMachine(machineID)`.
3. For each set: `DeleteExtensionsConfigurationForMachineSet(setID)`, then
   `DeleteMachineSet(setID)`.
4. Finally `DeleteCluster(clusterName)`.

`status.AllocatedMachines` is no longer consulted during deletion (it remains as
informational status). Update the function's doc comment accordingly.

## Tests

Add a test in `controllers/omnicluster_controller_test.go` using the in-memory
state:

- Create an Omni `Cluster`, two `MachineSet`s (control-plane and worker, with the
  `LabelCluster` label), `MachineSetNode`s bound to each, a `ConfigPatch` per
  machine, a `KernelArgs` per machine, and an `ExtensionsConfiguration` per set —
  reuse the existing `Ensure*` client methods to build this fixture.
- Call `deleteOmniResources` with an `OmniCluster` whose
  `Status.AllocatedMachines` is **empty** (the failure mode being fixed).
- Assert every resource above is gone from the state afterwards.

## Constraints

- Do NOT commit. Do NOT touch `metaPrompts/` or unrelated files.

## Acceptance Criteria

- `gofmt -l .` clean; `go vet ./...`, `go build ./...`, `go test ./...` pass.
- Teardown succeeds with an empty/stale status map.
