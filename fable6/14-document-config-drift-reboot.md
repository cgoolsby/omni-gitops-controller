# Prompt 14 — Document Config Drift Detection and Safe Rolling Reboot

## Context

The controller's most operationally significant feature has zero README
coverage: config changes declared in Git automatically propagate to running
Talos machines via drift detection and controlled reboots.

**Document the CURRENT behaviour — read the code first.** The implementation in
`controllers/omnicluster_controller.go` and `controllers/omni_client.go`
(`GetDriftingMachines`, `selectRebootCandidate`, `rebootCooldown`,
`status.lastRebootTimes`) includes safety machinery that older descriptions of
this controller lack. Do not describe it from memory of other docs.

## What to Do

Add a `## Config Drift Detection` section to `README.md` after the ArgoCD
Integration section and before Examples, covering:

1. **What triggers drift** — changing `configPatches` (and other config-level
   spec fields) causes a machine's running config to fall behind the desired
   config (`ConfigUpToDate == false` in Omni).
2. **Detection** — each reconcile of a Ready cluster queries Omni's
   `ClusterMachineStatus` resources for drifting machines.
3. **Cluster-wide health gate** — reboots are only considered when **every**
   machine in the cluster is RUNNING and Ready. This is what enforces "at most
   one machine down at a time": while a rebooted machine is still coming back,
   no further reboot is issued. In an HA control plane this preserves etcd
   quorum.
4. **Reboot cooldown** — each machine's last reboot attempt is recorded in
   `status.lastRebootTimes`; a machine is not rebooted again within the cooldown
   (state the constant from the code, currently 10 minutes). A machine that
   stays drifting after a reboot surfaces via the `ConfigDriftDetected`
   condition with reason `RebootCooldown` instead of being reboot-looped.
5. **Conditions** — document `ConfigDriftDetected`:
   - `True` / `PendingReboot` — drifting machine found, reboot being issued
   - `True` / `RebootCooldown` — drifting machine(s) waiting out the cooldown
   - `False` / `AllMachinesUpToDate` — no drift
6. **Observing** — `kubectl describe omnicluster <name>` /
   `-o jsonpath='{.status.conditions}'`, plus `status.lastRebootTimes`.
7. **Limitations** — no opt-out flag yet (reboot is always automatic when drift
   is detected and the gate passes); schematic-level changes (extensions /
   kernel args) are applied by Omni's own upgrade flow, and a plain reboot may
   not be sufficient for them — be honest about this boundary if the code
   doesn't special-case it.

Keep the section consistent with the README's existing voice and formatting.

## Verification

```bash
grep -n "Config Drift\|RebootCooldown\|lastRebootTimes" README.md
```

Every claim in the section must be checkable against the current code.

## Constraints

- Do NOT commit. Do NOT touch `metaPrompts/` or unrelated files.

## Acceptance Criteria

- Section present, accurate to the current implementation; go gates pass
  (unchanged).
