# Prompt 01 — Gate Drift Reboots on Cluster-Wide Machine Health

## Context

The controller detects config drift and reboots one drifting machine per reconcile
("rolling window = 1", see `controllers/omnicluster_controller.go` around line 183).
The window is **not actually enforced**:

`GetDriftingMachines` (`controllers/omni_client.go`) only checks that the *drifting*
machine itself is `RUNNING`, `Ready`, and has no config error. It never checks the
health of the **other** machines in the cluster. Failure sequence in a 3-node HA
control plane:

1. Machine A is drifting → controller reboots A.
2. 60s later the requeue fires. A is mid-reboot, so it is *excluded* from the drift
   list (not RUNNING). Machine B is RUNNING and drifting → controller reboots B
   **while A is still down**. Two etcd members down → quorum lost.

`status.Ready` (KubernetesAPIReady) does not protect against this: the API server
can remain reachable with one control-plane node down.

## What to Do

In `controllers/omni_client.go`, change `GetDriftingMachines` so that it first
verifies **every** `ClusterMachineStatus` in the cluster is healthy, and returns an
empty list (no reboot candidates) if any machine is not. "Healthy" means:
`Stage == ClusterMachineStatusSpec_RUNNING && Ready == true`.

Implementation sketch:

1. List all `ClusterMachineStatus` resources labelled with the cluster (the function
   already does this).
2. First pass: if any machine in the list is not RUNNING+Ready, return `nil, nil` —
   it is not safe to take another machine down this cycle.
3. Second pass (only when all machines are healthy): collect drifting machines as
   today (`!ConfigUpToDate && LastConfigError == ""`).
4. Update the doc comment to state the cluster-wide health precondition, and update
   the "rolling window" comment in `omnicluster_controller.go` to explain that the
   window is enforced by refusing to reboot while any machine is unhealthy.

## Tests

Add unit tests in `controllers/omnicluster_controller_test.go` using the existing
in-memory COSI state helpers (`newTestState` / `newTestOmniClient`):

- **All healthy, one drifting** → that machine is returned.
- **One machine unhealthy** (e.g. a non-RUNNING stage or `Ready == false`) while
  another healthy machine is drifting → empty result.

To build fixtures, create `ClusterMachineStatus` resources in the in-memory state
with the label `omnires.LabelCluster = <cluster>` and set `ConfigUpToDate`, `Stage`,
`Ready`, and `ManagementAddress` on the typed spec. Check the constructor signature
in the vendored `github.com/siderolabs/omni/client/pkg/omni/resources/omni` package
(pattern matches the other `NewXxx` constructors used in `omni_client.go`).

## Constraints

- Do NOT commit. The runner script commits after verification.
- Do NOT touch `metaPrompts/` or unrelated files.

## Acceptance Criteria

- `gofmt -l .` reports nothing, `go vet ./...`, `go build ./...`, and
  `go test ./...` all pass.
- New tests cover both the healthy-cluster and unhealthy-cluster cases.
