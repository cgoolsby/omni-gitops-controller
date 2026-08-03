# Prompt 03 — Control-Plane Scale-Down: One Member Per Reconcile

## Context

In `reconcileMachineSet` (`controllers/omnicluster_controller.go`), the scale-down
path computes `toRemove := -needed` and evicts that many machines in a single pass.
Scaling a control plane from 5 → 3 replicas removes **two etcd members
simultaneously**, which risks quorum loss while Omni is still processing the first
removal.

## What to Do

In the scale-down branch of `reconcileMachineSet`:

- When `role == omnires.LabelControlPlaneRole`, cap `toRemove` at **1** per
  reconcile. The periodic requeue (the reconciler already returns
  `RequeueAfter`) will remove the next member on a later cycle, after Omni has
  finished tearing down the previous one (`DeleteMachineSetNode` already returns
  an error while teardown is in progress, which causes a retry).
- Add a comment explaining the etcd-safety rationale.
- Worker scale-down behaviour stays unchanged (batch removal is fine for
  workers).

## Tests

Add a unit test in `controllers/omnicluster_controller_test.go` using the
in-memory state helpers:

- Pre-create 3 `MachineSetNode`s bound to a control-plane machine set (use
  `setupAllocatedMachine` with `omnires.LabelControlPlaneRole`).
- Call `reconcileMachineSet` with `Replicas: 1` and the control-plane role.
- Assert exactly **2** machines remain after the first call (one removed), and
  exactly **1** remains after a second call.
- A parallel worker-role test asserting batch removal still happens in one call
  (3 → 1 in a single invocation).

## Constraints

- Do NOT commit. Do NOT touch `metaPrompts/` or unrelated files.

## Acceptance Criteria

- `gofmt -l .` clean; `go vet ./...`, `go build ./...`, `go test ./...` pass.
