# Prompt 08 — Add Kubernetes Events Emission

## Context

The controller's RBAC (`config/rbac/clusterrole.yaml`) already grants `events: create, patch`, and `charts/omni-gitops-controller/templates/clusterrole.yaml` mirrors this. However, the controller never calls `r.Recorder.Event(...)` — no events are ever emitted.

Kubernetes Events are surfaced by `kubectl describe omnicluster <name>` and consumed by monitoring systems. Without them, debugging a stuck or failing `OmniCluster` requires reading controller pod logs directly.

The `OmniClusterReconciler` struct currently has no `Recorder` field.

---

## What to Do

### 1. Add a `Recorder` field to the reconciler

In `controllers/omnicluster_controller.go`:

```go
type OmniClusterReconciler struct {
    client.Client
    Scheme              *runtime.Scheme
    OmniClient          *OmniClient
    KubeconfigNamespace string
    ArgoCDClusters      bool
    Recorder            record.EventRecorder  // add this
}
```

Import `"k8s.io/client-go/tools/record"`.

### 2. Wire the recorder in `main.go`

```go
if err = (&controllers.OmniClusterReconciler{
    Client:              mgr.GetClient(),
    Scheme:              mgr.GetScheme(),
    OmniClient:          omniClient,
    KubeconfigNamespace: kubeconfigNamespace,
    ArgoCDClusters:      argoCDClusters,
    Recorder:            mgr.GetEventRecorderFor("omni-gitops-controller"),
}).SetupWithManager(mgr); err != nil {
```

### 3. Emit events at key transitions

Add `r.Recorder.Event(cluster, ...)` calls at these points in `omnicluster_controller.go`:

| Location | Event type | Reason | Message |
|---|---|---|---|
| After `EnsureCluster` succeeds (first time — check if cluster was just created) | `Normal` | `ClusterProvisioned` | `"Omni cluster created"` |
| After all `reconcileMachineSet` calls succeed | `Normal` | `MachinesAllocated` | `"Allocated N machines to machine set X"` |
| After `ensureKubeconfigSecret` / `ensureArgoCDClusterSecret` succeeds | `Normal` | `KubeconfigWritten` | `"Kubeconfig written to <namespace>/<name>-kubeconfig"` |
| In `setFailure` | `Warning` | the `reason` string | the error message |
| When drift is detected and reboot is triggered | `Warning` | `RebootTriggered` | `"Triggering reboot on machine <id> to apply config update"` |
| In `deleteOmniResources` when teardown completes | `Normal` | `ClusterDeleted` | `"Omni cluster resources deleted"` |

Use `corev1.EventTypeNormal` and `corev1.EventTypeWarning` constants.

---

## Verification

```bash
kubectl describe omnicluster <name> -n omni-gitops-system
# Events section should show provisioning timeline
```

Confirm events appear under the `Events:` section of the describe output at each lifecycle stage.
