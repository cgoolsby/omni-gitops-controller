# Prompt 07 — Fix README Conditions Table (Lists Non-Existent Conditions)

## Context

The README's `## OmniCluster API Reference` → `### Status Fields` table contains this row:

> `status.conditions[]` — Standard Kubernetes condition set. Condition types include `Ready`, `ClusterProvisioned`, `MachinesAllocated`, `KubeconfigWritten`.

However, the controller (`omnicluster_controller.go`) only ever calls `setCondition` with two condition types:
- `Ready`
- `ConfigDriftDetected`

`ClusterProvisioned`, `MachinesAllocated`, and `KubeconfigWritten` are **never set** by the code. A user who adds these to an alert rule or automation will wait forever for conditions that never appear.

---

## What to Do

### Option A: Fix the README to reflect reality (minimum)

Update the `status.conditions[]` row to accurately list only the conditions the controller actually sets:

| Field | Type | Description |
|---|---|---|
| `status.conditions[Ready]` | `metav1.Condition` | `True` when Omni reports the cluster as Running and the Kubernetes API is reachable. Reason mirrors the Omni cluster phase (e.g. `Running`, `ScalingUp`). |
| `status.conditions[ConfigDriftDetected]` | `metav1.Condition` | `True` (reason: `PendingReboot`) when one or more machines have a pending config update. `False` (reason: `AllMachinesUpToDate`) when all machines are current. Only evaluated when `Ready` is `True`. |

### Option B: Implement the missing conditions (enhancement)

If the three additional conditions are genuinely useful, implement them in the reconcile loop:

- **`ClusterProvisioned`**: Set to `True` after `EnsureCluster` succeeds; `False` with the failure reason if it fails.
- **`MachinesAllocated`**: Set to `True` after all `reconcileMachineSet` calls succeed; `False` if any machine set can't be fully allocated (e.g., insufficient available machines).
- **`KubeconfigWritten`**: Set to `True` after `ensureKubeconfigSecret` / `ensureArgoCDClusterSecret` succeeds; `False` on failure.

These would make the status more granular and useful for automation (e.g., waiting for `KubeconfigWritten` before deploying workloads).

---

## Recommendation

Do Option A immediately (it's a one-line docs fix) and track Option B as a separate enhancement issue.

---

## Verification

- `grep -n "ClusterProvisioned\|MachinesAllocated\|KubeconfigWritten" README.md` returns no results after the fix (Option A) or returns results in both README and the controller code (Option B).
- Read the conditions section aloud and confirm every listed condition type exists in `omnicluster_controller.go`.
