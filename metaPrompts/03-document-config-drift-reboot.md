# Prompt 03 — Document Config Drift Detection and Rolling Reboot

## Context

The controller implements automatic config drift detection and rolling machine reboot:

- `GetDriftingMachines` (in `omni_client.go`) queries `ClusterMachineStatus` resources and returns machines where `ConfigUpToDate == false` while the machine is `RUNNING`, `Ready`, and has no config error.
- The reconciler (in `omnicluster_controller.go`) calls this after the cluster is Ready, then reboots the **first** drifting machine via `RebootMachine`. Only one machine is rebooted per reconcile cycle (rolling window = 1) to prevent simultaneous reboots in HA clusters.
- Two conditions track this: `Ready` and `ConfigDriftDetected`.

This is one of the most operationally significant features — it means Talos config changes declared in Git automatically propagate to running machines without manual intervention. However, the README has **zero mention** of it.

---

## What to Do

Add a new section `## Config Drift Detection` to `README.md`, placed after the `## ArgoCD Integration` section and before `## Examples`. The section should explain:

1. **What triggers drift**: Changing `configPatches`, `machineExtensions`, or `kernelArgs` in the `OmniCluster` spec. When the controller applies a new patch or extension, the Omni machine's running config falls behind the desired config.

2. **How detection works**: On each reconcile cycle (when `status.ready == true`), the controller queries Omni's `ClusterMachineStatus` resources and identifies machines where the config is not up to date.

3. **What happens**: The controller reboots one machine per reconcile cycle (rolling window = 1). After the machine reboots and rejoins, the next reconcile cycle either picks the next drifting machine or sets `ConfigDriftDetected: False`.

4. **The conditions**: Describe both condition types:
   - `ConfigDriftDetected: "True"` with reason `PendingReboot` — a machine has a pending config update; reboot is scheduled.
   - `ConfigDriftDetected: "False"` with reason `AllMachinesUpToDate` — all machines are running the target config.

5. **HA safety**: In a 3-node HA control plane, rebooting one node at a time preserves etcd quorum. The rolling window of 1 is intentional.

6. **Observing drift**:

```bash
kubectl get omnicluster <name> -n omni-gitops-system -o jsonpath='{.status.conditions}'
# or
kubectl describe omnicluster <name> -n omni-gitops-system
```

7. **Disabling automatic reboot** (if a user wants to apply changes manually): note that there is currently no opt-out flag — the reboot is always triggered when drift is detected. (This is a known limitation; see the issues tracker.)

Also add a `ConfigDriftDetected` entry to the **Status Fields** table in `## OmniCluster API Reference`:

| `status.conditions[ConfigDriftDetected]` | `metav1.Condition` | `True` when one or more machines have a pending config update. Reason is `PendingReboot`; message names the first drifting machine. `False` with reason `AllMachinesUpToDate` when all machines are current. |

---

## Verification

- `grep -n "Config Drift\|ConfigDriftDetected\|rolling reboot" README.md` returns results in a new section.
- The section appears in the rendered README between ArgoCD Integration and Examples.
- A reader unfamiliar with the codebase can understand the full lifecycle from reading the section alone.
