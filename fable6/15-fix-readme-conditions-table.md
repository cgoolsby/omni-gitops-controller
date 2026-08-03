# Prompt 15 — Fix README Conditions Table and Status Fields Reference

## Context

The README's `### Status Fields` table claims condition types
`ClusterProvisioned`, `MachinesAllocated`, and `KubeconfigWritten` exist. The
controller has **never set those conditions** — it sets exactly two:
`Ready` and `ConfigDriftDetected`. (Events with similar reasons may exist now —
events are not conditions; don't conflate them.) A user alerting on the phantom
conditions waits forever.

The status struct has also grown fields the README doesn't document.

## What to Do

Read `api/v1alpha1/types.go` (`OmniClusterStatus`) and
`controllers/omnicluster_controller.go` (every `meta.SetStatusCondition` call),
then make the README Status Fields section match reality exactly:

1. Replace the conditions row(s) with accurate entries:
   - `status.conditions[Ready]` — `True` when Omni reports the cluster Running
     and the Kubernetes API reachable; reason mirrors the Omni phase, or a
     failure reason (e.g. `EnsureClusterFailed`) when reconcile fails.
   - `status.conditions[ConfigDriftDetected]` — reasons `PendingReboot`,
     `RebootCooldown`, `AllMachinesUpToDate` (see the drift section added in the
     previous change; keep wording consistent with it).
2. Add rows for any undocumented status fields, e.g. `status.observedGeneration`
   (generation last processed), `status.lastRebootTimes` (per-machine reboot
   timestamps for the drift cooldown), `status.allocatedMachines`,
   `status.failureReason` / `status.failureMessage` — whatever the struct
   actually contains and the table currently lacks.
3. Confirm no other README section references the phantom condition types.

## Verification

```bash
grep -n "ClusterProvisioned\|MachinesAllocated\|KubeconfigWritten" README.md
# any remaining hits must refer to EVENT reasons (if documented), not conditions
```

Cross-check every condition type/reason named in the README against
`meta.SetStatusCondition` calls in the controller.

## Constraints

- Do NOT commit. Do NOT touch `metaPrompts/` or unrelated files.

## Acceptance Criteria

- README documents exactly the conditions and status fields the code produces;
  go gates pass (unchanged).
