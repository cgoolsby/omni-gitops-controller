# Prompt 02 — Per-Machine Reboot Cooldown

## Context

When config drift persists after a reboot (e.g. the machine comes back RUNNING and
Ready but `ConfigUpToDate` is still false, with no `LastConfigError`), the
reconciler in `controllers/omnicluster_controller.go` reboots the same machine
again on the next cycle — every ~60 seconds, forever. Nothing records that a
machine was recently rebooted.

## What to Do

### 1. Add a status field

In `api/v1alpha1/types.go`, add to `OmniClusterStatus`:

```go
// LastRebootTimes records when the controller last issued a drift-remediation
// reboot for each machine (keyed by machine UUID). Used to enforce a cooldown
// so a persistently-drifting machine is not rebooted in a loop.
// +optional
LastRebootTimes map[string]metav1.Time `json:"lastRebootTimes,omitempty"`
```

### 2. Enforce a cooldown in the reconciler

In `controllers/omnicluster_controller.go`:

- Add a constant `rebootCooldown = 10 * time.Minute`.
- Extract a small pure helper, e.g.
  `selectRebootCandidate(drifting []MachineConfigDrift, lastReboots map[string]metav1.Time, now time.Time) *MachineConfigDrift`,
  returning the first drifting machine whose last reboot is absent or older than
  the cooldown, or nil if all are cooling down.
- Restructure the reboot block: pick the candidate **before** the status update,
  record `LastRebootTimes[machineID] = now` in status (record the *attempt* time —
  even if the reboot RPC later fails, we don't want to hammer it), persist status,
  then issue the reboot.
- Prune `LastRebootTimes` entries for machines no longer present in
  `allocatedMachines` so the map doesn't grow forever.
- When machines are drifting but all are in cooldown, set the
  `ConfigDriftDetected` condition with reason `RebootCooldown` and a message that
  names the machine(s) waiting, so a stuck machine is visible to operators.

### 3. Regenerate generated artifacts

The status type changed, so regenerate:

```bash
go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest
controller-gen object paths="./api/..."
controller-gen crd:generateEmbeddedObjectMeta=true paths="./api/..." output:crd:artifacts:config=config/crd/bases
cp config/crd/bases/omni.gitops.dev_omniclusters.yaml charts/omni-gitops-controller/crds/
```

## Tests

Unit-test the `selectRebootCandidate` helper directly in
`controllers/omnicluster_controller_test.go`:

- No prior reboot recorded → first drifting machine returned.
- Last reboot 1 minute ago → skipped; a second drifting machine with no record is
  returned instead.
- All drifting machines within cooldown → nil.
- Last reboot older than the cooldown → machine is eligible again.

## Constraints

- Do NOT commit. Do NOT touch `metaPrompts/` or unrelated files.

## Acceptance Criteria

- `gofmt -l .` clean; `go vet ./...`, `go build ./...`, `go test ./...` pass.
- `config/crd/bases/` and the chart's `crds/` copy are regenerated and identical.
