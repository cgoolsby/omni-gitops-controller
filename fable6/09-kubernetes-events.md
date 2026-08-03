# Prompt 09 — Emit Kubernetes Events at Lifecycle Transitions

## Context

The RBAC (`config/rbac/clusterrole.yaml` and the chart's clusterrole) already
grants `events: create, patch`, but the controller never emits any —
`OmniClusterReconciler` has no `Recorder` field. Debugging a stuck `OmniCluster`
currently requires controller pod logs; `kubectl describe omnicluster` shows
nothing.

Note the current code shape (this differs from older descriptions of the
controller): `setFailure` takes a `statusBefore` argument; the reboot block uses
`selectRebootCandidate` with a cooldown; there is a cluster-ownership guard in
`EnsureCluster`.

## What to Do

### 1. Wire the recorder

- Add `Recorder record.EventRecorder` (`k8s.io/client-go/tools/record`) to
  `OmniClusterReconciler` in `controllers/omnicluster_controller.go`.
- In `main.go`, set `Recorder: mgr.GetEventRecorderFor("omni-gitops-controller")`.

### 2. Emit events (use `corev1.EventTypeNormal` / `EventTypeWarning`)

| Where | Type | Reason | Message |
|---|---|---|---|
| `setFailure` | Warning | the `reason` string | the error message |
| Reboot issued (after `RebootMachine` is called) | Normal | `RebootTriggered` | "Rebooting machine \<id\> to apply config update" |
| Reboot RPC fails | Warning | `RebootFailed` | the error |
| Drifting machines all in cooldown | Warning | `RebootCooldown` | machines waiting |
| Fresh machines bound in `reconcileMachineSet` | Normal | `MachinesAllocated` | "Allocated N machine(s) to \<machineSetID\>" — emit from the caller or pass the cluster object down; keep the signature change minimal |
| Kubeconfig Secret actually written (not the skipped-fresh path) | Normal | `KubeconfigWritten` | "Kubeconfig written to \<ns\>/\<name\>" — have the ensure functions report whether they wrote |
| `deleteOmniResources` completes | Normal | `ClusterDeleted` | "Omni cluster resources deleted" |

Guard all emissions with `if r.Recorder != nil` so existing unit tests that
construct `OmniClusterReconciler{OmniClient: c}` directly keep working without a
recorder.

## Tests

Existing tests must pass (nil-recorder guard). Optionally use
`record.NewFakeRecorder` in one test to assert an event is emitted on the
machines-allocated path — keep it lightweight.

## Constraints

- Do NOT commit. Do NOT touch `metaPrompts/` or unrelated files.

## Acceptance Criteria

- `gofmt -l .` clean; `go vet ./...`, `go build ./...`, `go test ./...` pass.
- Recorder wired in `main.go`; events emitted at the transitions above.
