# omni-gitops-controller

A Kubernetes operator that bridges [Siderolabs Omni](https://omni.siderolabs.com)
with [Flux CD](https://fluxcd.io) (and [ArgoCD](https://argoproj.github.io/cd/)).

[![Go Report Card](https://goreportcard.com/badge/github.com/cgoolsby/omni-gitops-controller)](https://goreportcard.com/report/github.com/cgoolsby/omni-gitops-controller)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

---

## What It Does

When you run self-hosted Omni, there is no declarative, GitOps-friendly way to provision
Talos clusters and have them automatically available to Flux for workload reconciliation.
You either provision clusters by hand through the Omni UI, or you write custom tooling to
call Omni's API and then separately distribute kubeconfigs to your GitOps tooling. Neither
approach scales, and neither is reproducible from a Git repo.

`omni-gitops-controller` solves this by introducing a single `OmniCluster` custom resource.
You declare the desired cluster — Kubernetes version, Talos version, machine selectors, and
optional config patches — in your GitOps repository. Flux applies it to your management
cluster, and the controller takes over: it calls Omni's native COSI gRPC API to create the
`Cluster`, `MachineSet`, `MachineSetNode`, and `ConfigPatch` resources, allocating machines
from Omni's inventory pool using the label selectors you specify.

Once the cluster is running, the controller retrieves the kubeconfig from Omni and writes it
as a Kubernetes `Secret` into the namespace of your choice (default: `flux-system`). Flux can
immediately target the new cluster using a `Kustomization` with a `kubeConfig.secretRef`
pointing at that secret — no manual steps, no out-of-band credential distribution.

---

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  Management Cluster (runs omni-gitops-controller)            │
│                                                              │
│  Git Repo ──Flux──▶ OmniCluster CR                          │
│                           │                                  │
│                    omni-gitops-controller                    │
│                           │                                  │
│              ┌────────────┴────────────┐                    │
│              ▼                         ▼                    │
│        Omni COSI API            flux-system/                │
│    (Cluster, MachineSet,    <name>-kubeconfig Secret         │
│     MachineSetNode,                   │                     │
│     ConfigPatch)                      │                     │
│              │                        │                     │
│              ▼                        ▼                     │
│       Talos machines            Flux targets                │
│       provision &               child cluster ─────────────▶│
│       join cluster              (remote apply)              │
└─────────────────────────────────────────────────────────────┘
```

The controller runs entirely on the management cluster. It holds no state of its own —
all cluster state lives in Omni (authoritative), in the `OmniCluster`/`Machine` custom
resources, and in their status subresources.

Per-machine Omni state is owned by a first-class, namespaced **`Machine`** CRD and its own
`MachineReconciler`. `OmniClusterReconciler` is an orchestrator: it decides which machines a
cluster needs and expresses that by creating/updating/deleting `Machine` objects — the same
shape as Deployment → ReplicaSet → Pod, or Cluster API's Cluster → MachineSet → Machine. Each
`Machine`'s `metadata.name` is the raw Omni machine UUID, and every `Machine` is owned
(controller reference) by its `OmniCluster`, so a machine's state change enqueues its owner via
`.Owns(&Machine{})` — no polling — and machines are garbage-collected on cluster delete.

### Controller responsibilities

| Concern | Owner |
|---|---|
| Cluster / MachineSet existence in Omni | `OmniClusterReconciler` |
| Extensions / schematic config (machine-set scoped in Omni) | `OmniClusterReconciler` |
| Which machines are allocated to a set; scale-up/down; quorum-safe one-at-a-time control-plane scale-down | `OmniClusterReconciler` — expressed as create/update/delete of `Machine` objects; "already allocated" is a labelled `List` (`omni.gitops.dev/cluster`, `omni.gitops.dev/machine-set`) |
| Keeping already-allocated `Machine` specs in sync when the machine-set spec changes | `OmniClusterReconciler` |
| Cross-machine drift detection + reboot-candidate selection ("skip if any machine unhealthy", one reboot in flight cluster-wide, per-machine cooldown) | `OmniClusterReconciler` — sets `Machine.Spec.RebootRequestedAt` on the chosen candidate |
| Bind/unbind a machine to its set in Omni | `MachineReconciler` |
| Config patches + kernel args for one machine | `MachineReconciler` |
| Issuing the reboot RPC and recording the outcome | `MachineReconciler` — `Status.LastRebootTime` is stamped only *after* the RPC returns |
| Per-machine teardown on release/delete (unbind → config patches → kernel args) | `MachineReconciler` — via the `machine.omni.gitops.dev/finalizer` |

A reboot is "owed" iff `Machine.Spec.RebootRequestedAt` is newer than `Status.LastRebootTime`;
stamping `LastRebootTime` after the RPC naturally makes it no longer owed, so no field-clearing
handshake is needed.

Deletion is handled via finalizers: the cluster controller deletes its `Machine` objects and
waits (via the `.Owns` watch) for each finalizer to unwind its per-machine Omni state before
tearing down the machine-set-scoped and cluster-scoped Omni resources, preventing orphaned
machines in Omni.

---

## Why Not CAPI?

- **Complexity:** CAPI requires 3–5 separate controllers (core, bootstrap, infrastructure,
  control-plane) and a hierarchy of CRDs (`Cluster`, `Machine`, `MachineSet`,
  `BootstrapConfig`, `InfrastructureMachine`, etc.). This controller is a single binary,
  one CRD, and ~600 lines of Go.
- **No native Omni backend:** Using CAPI with Talos requires `cluster-api-provider-talos`,
  which provisions machines directly via `talosctl`. It bypasses Omni entirely, so Omni is
  not the source of truth for machine inventory or lifecycle.
- **Protocol alignment:** This controller speaks Omni's native COSI gRPC protocol. Omni
  remains authoritative for machine registration, labeling, OS upgrades, and health
  monitoring. The controller is an orchestration layer on top of Omni, not a replacement.
- **Operational simplicity:** One `helm install` or `kubectl apply -k`. No provider
  infrastructure, no bootstrap pivot, no CAPI management cluster bootstrap sequence.
- **Choose CAPI** if you need portability across infrastructure providers (AWS, Azure, GCP,
  vSphere). **Choose this controller** if Omni is your platform and you want a minimal,
  Omni-native GitOps integration.

---

## Prerequisites

- A Kubernetes cluster to run the controller on (the "management cluster")
- [Siderolabs Omni](https://omni.siderolabs.com) (self-hosted), reachable from the
  management cluster
- An Omni service account token (`omnictl serviceaccount create`)
- Flux CD installed on the management cluster (for the GitOps workflow)
- Physical or virtual machines already registered in Omni

---

## Quick Start

### 1. Get your service account token

```bash
omnictl serviceaccount create flux-controller \
  --use-user-role=true \
  --role=Admin
```

Copy the base64-encoded key from the output.

### 2. Install the CRD and controller

**Using Helm (recommended):**

```bash
helm install omni-gitops-controller \
  oci://ghcr.io/cgoolsby/charts/omni-gitops-controller \
  --namespace omni-gitops-system --create-namespace \
  --set omni.endpoint=https://omni.example.com \
  --set omni.token=<your-service-account-token>
```

To use an existing Secret instead of passing the token inline:

```bash
# Secret must have keys: endpoint, serviceAccountToken
kubectl create secret generic omni-credentials \
  --namespace omni-gitops-system \
  --from-literal=endpoint=https://omni.example.com \
  --from-literal=serviceAccountToken=<your-service-account-token>

helm install omni-gitops-controller \
  oci://ghcr.io/cgoolsby/charts/omni-gitops-controller \
  --namespace omni-gitops-system --create-namespace \
  --set omni.existingSecret=omni-credentials
```

**Using Kustomize:**

```bash
# Create the credentials secret first
kubectl create secret generic omni-credentials \
  --namespace omni-gitops-system \
  --from-literal=endpoint=https://omni.example.com \
  --from-literal=serviceAccountToken=<your-service-account-token>

kubectl apply -k https://github.com/cgoolsby/omni-gitops-controller/config/default
```

**Verifying the image (optional):**

Release images are signed with [cosign](https://github.com/sigstore/cosign)
keyless signing via GitHub OIDC, and ship with an attached CycloneDX SBOM.
To verify a release image:

```bash
cosign verify ghcr.io/cgoolsby/omni-gitops-controller:<version> \
  --certificate-identity-regexp='^https://github.com/cgoolsby/omni-gitops-controller/\.github/workflows/release\.yml@refs/tags/v.*$' \
  --certificate-oidc-issuer=https://token.actions.githubusercontent.com
```

To download the SBOM:

```bash
cosign download sbom ghcr.io/cgoolsby/omni-gitops-controller:<version> > sbom.cyclonedx.json
```

### 3. Declare your first cluster

```yaml
apiVersion: omni.gitops.dev/v1alpha1
kind: OmniCluster
metadata:
  name: my-cluster
  namespace: omni-gitops-system
spec:
  kubernetesVersion: "1.31.0"
  talosVersion: "v1.9.0"
  controlPlane:
    replicas: 1
    machineSelector:
      matchLabels: {}   # empty = any available machine
```

### 4. Watch it provision

```bash
kubectl get omniclusters -n omni-gitops-system -w
# NAME         K8S      TALOS    CP   READY   PHASE
# my-cluster   1.31.0   v1.9.0   1    false   ScalingUp
# my-cluster   1.31.0   v1.9.0   1    true    Running
```

Once `READY=true`, the kubeconfig is written to `flux-system/my-cluster-kubeconfig`.
Flux can immediately target the cluster using a `Kustomization` with
`spec.kubeConfig.secretRef.name: my-cluster-kubeconfig`.

---

## OmniCluster API Reference

### Spec Fields

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `spec.kubernetesVersion` | `string` | yes | — | Target Kubernetes version, e.g. `"1.31.0"`. Do not include the `v` prefix. |
| `spec.talosVersion` | `string` | yes | — | Target Talos Linux version, e.g. `"v1.9.0"`. Must include the `v` prefix. |
| `spec.controlPlane` | `MachineSetSpec` | yes | — | Describes the control-plane machine set. |
| `spec.controlPlane.replicas` | `int32` | no | `1` | Number of control-plane machines. Use `1` or an odd number ≥ 3 for etcd quorum. |
| `spec.controlPlane.machineSelector` | `LabelSelector` | yes | — | Selects available Omni machines by label. Supports both `matchLabels` and `matchExpressions`. Empty `matchLabels: {}` matches any available machine. |
| `spec.controlPlane.configPatches[]` | `[]ConfigPatch` | no | — | Talos machine config patches applied to every control-plane machine. |
| `spec.controlPlane.configPatches[].name` | `string` | yes | — | Unique identifier for this patch within the machine set. |
| `spec.controlPlane.configPatches[].inline` | `JSON` | yes | — | Patch content in Talos machine config YAML/JSON format. |
| `spec.controlPlane.machineExtensions[]` | `[]string` | no | — | Talos system extension IDs (e.g. `siderolabs/nvidia-open-gpu-kernel-modules`). Creates/updates an `ExtensionsConfiguration` scoped to the machine set; clearing the list deletes it so the schematic reverts. |
| `spec.controlPlane.kernelArgs[]` | `[]string` | no | — | Extra kernel args applied at the **schematic** level (active during maintenance/install boot, e.g. `libata.force=noncq`). Clearing the list removes the per-machine `KernelArgs` resource; evicted machines also release theirs. |
| `spec.workers[]` | `[]WorkerMachineSetSpec` | no | — | Additional worker machine sets. Omit for single-node clusters. |
| `spec.workers[].name` | `string` | yes | — | Identifies the worker set. Becomes the Omni MachineSet suffix: `<cluster>-<name>`. |
| `spec.workers[].replicas` | `int32` | no | `1` | Number of worker machines in this set. |
| `spec.workers[].machineSelector` | `LabelSelector` | yes | — | Selects available machines for this worker set. |
| `spec.workers[].configPatches[]` | `[]ConfigPatch` | no | — | Config patches applied to every machine in this worker set. |
| `spec.workers[].machineExtensions[]` | `[]string` | no | — | Talos system extension IDs for this worker set. Same semantics as `spec.controlPlane.machineExtensions[]`. |
| `spec.workers[].kernelArgs[]` | `[]string` | no | — | Extra schematic-level kernel args for this worker set. Same semantics as `spec.controlPlane.kernelArgs[]`. |

### Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `status.observedGeneration` | `int64` | The `.metadata.generation` last processed by the controller. Set on both success and failure paths; conditions carry the success/failure signal. |
| `status.phase` | `string` | Mirrors the Omni ClusterStatus phase: `ScalingUp`, `Running`, `ScalingDown`, `Destroying`, `Failed`. |
| `status.ready` | `bool` | `true` when Omni reports the cluster as `Running` and the Kubernetes API is reachable. |
| `status.allocatedMachines` | `map[string][]string` | Machine UUIDs allocated to this cluster, keyed by MachineSet ID. Derived from the owned `Machine` objects; informational. |
| `status.failureReason` | `string` | Short machine-readable token when `phase` is `Failed`, e.g. `EnsureClusterFailed`. |
| `status.failureMessage` | `string` | Human-readable error description when `phase` is `Failed`. |
| `status.conditions[Ready]` | `metav1.Condition` | `True` when Omni reports the cluster `Running` and the Kubernetes API is reachable. The reason mirrors the Omni phase (e.g. `Running`, `ScalingUp`), or is a failure reason (e.g. `EnsureClusterFailed`) when reconcile fails. |
| `status.conditions[ConfigDriftDetected]` | `metav1.Condition` | `True` with reason `PendingReboot` or `RebootCooldown` when machines are drifting; `False` with reason `AllMachinesUpToDate` when all machines run the target config. See [Config Drift Detection](#config-drift-detection). |

---

## Machine Label Selectors

Omni automatically applies labels to registered machines under the `omni.sidero.dev/` prefix.
Use these labels in `machineSelector` to target specific hardware. Both `matchLabels` and
`matchExpressions` (operators `In`, `NotIn`, `Exists`, `DoesNotExist`) are supported, with
standard Kubernetes label-selector semantics; when both are given they are ANDed together.

| Label | Example value | Description |
|-------|--------------|-------------|
| `omni.sidero.dev/available` | `""` | Set when a machine is not currently assigned to any cluster. **You do not need to include this** — the controller always adds it to every allocation query. |
| `omni.sidero.dev/mem` | `"32768"` | Total RAM in MiB. |
| `omni.sidero.dev/cpu` | `"16"` | Logical CPU count. |
| `omni.sidero.dev/platform` | `"metal"` | Platform type (`metal`, `aws`, `gcp`, etc.). |

**Example: select bare-metal machines with 64 GiB RAM for control-plane, 32 GiB for workers:**

```yaml
spec:
  controlPlane:
    replicas: 3
    machineSelector:
      matchLabels:
        omni.sidero.dev/platform: metal
        omni.sidero.dev/mem: "65536"
  workers:
    - name: general
      replicas: 3
      machineSelector:
        matchLabels:
          omni.sidero.dev/platform: metal
          omni.sidero.dev/mem: "32768"
```

**Example: use `matchExpressions` to target either of two platforms while excluding GPU machines:**

```yaml
spec:
  controlPlane:
    replicas: 3
    machineSelector:
      matchExpressions:
        - key: omni.sidero.dev/platform
          operator: In
          values: ["metal", "aws"]
        - key: gpu
          operator: DoesNotExist
```

The controller automatically appends `omni.sidero.dev/available` to every machine query —
you never need to include it in your selector.

---

## Controller Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--kubeconfig-namespace` | `flux-system` | Namespace where cluster kubeconfig Secrets are written. |
| `--argocd-clusters` | `false` | Write Secrets in ArgoCD cluster Secret format (adds `argocd.argoproj.io/secret-type: cluster` label and ArgoCD-specific data keys). |
| `--leader-elect` | `false` | Enable leader election for high-availability deployments. Always enable in production. |
| `--metrics-bind-address` | `:8080` | Address for the Prometheus metrics endpoint. |
| `--health-probe-bind-address` | `:8081` | Address for liveness and readiness probes. |

**Required environment variables:**

| Variable | Description |
|----------|-------------|
| `OMNI_ENDPOINT` | Omni HTTPS URL, e.g. `https://omni.example.com`. |
| `OMNI_SERVICE_ACCOUNT_TOKEN` | Base64-encoded service account token from `omnictl serviceaccount create`. |

---

## Flux Integration

After an `OmniCluster` reaches `Ready=true`, the controller writes the kubeconfig to
`<kubeconfig-namespace>/<cluster-name>-kubeconfig`. Point one or more Flux `Kustomization`
resources at the secret to deploy workloads onto the new cluster:

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: my-cluster-global
  namespace: flux-system
spec:
  interval: 10m
  path: ./clusters/my-cluster/global
  prune: true
  wait: true
  sourceRef:
    kind: GitRepository
    name: flux-system
  kubeConfig:
    secretRef:
      name: my-cluster-kubeconfig   # written by omni-gitops-controller
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: my-cluster-system
  namespace: flux-system
spec:
  interval: 10m
  path: ./clusters/my-cluster/system
  prune: true
  dependsOn:
    - name: my-cluster-global
  sourceRef:
    kind: GitRepository
    name: flux-system
  kubeConfig:
    secretRef:
      name: my-cluster-kubeconfig
```

See [`examples/flux-kustomization.yaml`](examples/flux-kustomization.yaml) for the full
layered example.

---

## ArgoCD Integration

Start the controller with `--argocd-clusters=true` and `--kubeconfig-namespace=argocd`.
Instead of writing a raw kubeconfig Secret, the controller writes a Secret in ArgoCD cluster
format:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: my-cluster
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: cluster
type: Opaque
data:
  name: <base64 cluster name>
  server: <base64 API server URL>
  config: <base64 JSON ArgoCD cluster config with bearerToken>
```

ArgoCD watches for Secrets with the `argocd.argoproj.io/secret-type: cluster` label in its
namespace and automatically registers the cluster. No manual `argocd cluster add` required.

**Helm install for ArgoCD:**

```bash
helm install omni-gitops-controller \
  oci://ghcr.io/cgoolsby/charts/omni-gitops-controller \
  --namespace omni-gitops-system --create-namespace \
  --set omni.endpoint=https://omni.example.com \
  --set omni.token=<token> \
  --set kubeconfigNamespace=argocd \
  --set argoCDClusters=true
```

See [`examples/argocd-cluster.yaml`](examples/argocd-cluster.yaml) for a complete
`OmniCluster` example targeting ArgoCD.

---

## Config Drift Detection

Config changes declared in Git propagate to running machines automatically. When you change
`configPatches` (or other config-level spec fields) on an `OmniCluster`, the controller pushes
the new desired config to Omni, and each machine's running config falls behind — Omni reports
`ConfigUpToDate: false` for that machine. Applying the new config to a Talos machine requires a
reboot, which is performed one machine at a time.

**Detection.** On every reconcile of a Ready cluster, the `OmniClusterReconciler` lists Omni's
`ClusterMachineStatus` resources for the cluster and collects machines whose config is out of
date (and that have no config error — a machine with a `LastConfigError` is never rebooted,
since rebooting won't fix a bad config).

**Cluster-wide health gate.** Reboots are only considered when **every** machine in the cluster
is in the `RUNNING` stage and Ready. If any machine is unhealthy — including one still coming
back from a previous drift-correction reboot — no reboot candidates are offered that cycle.
This is what enforces "at most one machine down at a time": the next reboot is not issued until
the previous machine has fully rejoined. In an HA control plane this preserves etcd quorum.

**Signal, then act.** The cluster controller selects at most one candidate cluster-wide and
signals the reboot by setting `RebootRequestedAt` on that `Machine`'s spec — it does not call
the reboot RPC itself. The `MachineReconciler` sees the request (a reboot is "owed" iff
`Spec.RebootRequestedAt` is newer than `Status.LastRebootTime`), issues the reboot RPC, and
stamps `Machine.Status.LastRebootTime` **only after the RPC returns** — so the stamp is never
persisted without a matching attempt, and stamping it makes the reboot naturally no longer owed
(no field-clearing handshake). A reboot already requested but not yet stamped counts as "in
flight" and suppresses a second request cluster-wide.

**Reboot cooldown.** The cluster controller reads each `Machine`'s `Status.LastRebootTime` and
will not request another reboot for that machine within **10 minutes** of its last attempt. If
a machine is still drifting inside that window — for example, the config genuinely requires more
than a reboot — the controller surfaces it via the `RebootCooldown` condition and a Warning
event instead of reboot-looping it.

**Conditions.** Drift state is reported on the `OmniCluster`'s `ConfigDriftDetected` condition:

| Status | Reason | Meaning |
|--------|--------|---------|
| `True` | `PendingReboot` | A drifting machine was found and a reboot has been requested / is in flight. |
| `True` | `RebootCooldown` | Drifting machine(s) were rebooted recently and are waiting out the cooldown. |
| `False` | `AllMachinesUpToDate` | All machines are running the target config. |

**Observing:**

```bash
kubectl describe omnicluster my-cluster -n omni-gitops-system

kubectl get omnicluster my-cluster -n omni-gitops-system \
  -o jsonpath='{.status.conditions}'

# Per-machine state, including the last drift-remediation reboot
kubectl get machines.omni.gitops.dev -n omni-gitops-system \
  -l omni.gitops.dev/cluster=my-cluster
```

The cluster controller emits a `RebootRequested` / `RebootCooldown` event on the `OmniCluster`,
and the `MachineReconciler` emits `RebootTriggered` / `RebootFailed` events on the `Machine`.

**Limitations:**

- There is no opt-out flag yet — when drift is detected and the health gate passes, the reboot
  is always automatic. If you need a manual maintenance window, don't push the config change
  until you're ready for the rolling reboot.
- Schematic-level changes (`machineExtensions`, `kernelArgs`) are written as Omni
  `ExtensionsConfiguration` / `KernelArgs` resources, and Omni applies them through its own
  upgrade flow. The drift-reboot path does not special-case them: if a schematic change leaves
  a machine drifting in a way a plain reboot cannot resolve, the machine will surface under
  `RebootCooldown` until Omni's upgrade completes.

---

## Examples

| File | Description |
|------|-------------|
| [`examples/single-node.yaml`](examples/single-node.yaml) | Single control-plane node, no workers. |
| [`examples/ha-control-plane.yaml`](examples/ha-control-plane.yaml) | Three-node HA control plane + worker pool, selected by RAM label. |
| [`examples/with-config-patches.yaml`](examples/with-config-patches.yaml) | Config patches for KubePrism, sysctl tuning, and GPU kernel modules. |
| [`examples/with-extensions-and-kernel-args.yaml`](examples/with-extensions-and-kernel-args.yaml) | GPU worker pool with NVIDIA system extensions, schematic-level kernel args, and a config patch loading the nvidia modules. |
| [`examples/flux-kustomization.yaml`](examples/flux-kustomization.yaml) | Flux `Kustomization` resources targeting a provisioned cluster. |
| [`examples/argocd-cluster.yaml`](examples/argocd-cluster.yaml) | `OmniCluster` for use with `--argocd-clusters`. |

---

## Troubleshooting

**`EnsureClusterFailed`: Omni endpoint unreachable**

The controller cannot reach the Omni API. Check the `OMNI_ENDPOINT` environment variable and
verify that the management cluster can route HTTPS traffic to Omni. If you use a self-signed
CA, the controller's Pod must trust it — mount the CA into the container and set
`SSL_CERT_FILE` or `SSL_CERT_DIR`.

**`AllocateCPMachinesFailed: not enough available machines`**

No machines in Omni match the `machineSelector` labels, or all matching machines are already
allocated to other clusters. Run `omnictl get machines` to inspect available machines and
their labels. Check that `omni.sidero.dev/available` is set on the machines you expect to
be free.

**`EnsureKubeconfigFailed`: kubeconfig fetch failed**

The Omni service account lacks permission to retrieve the cluster's kubeconfig. In the Omni
UI, verify that the service account was created with `--role=Admin` or at minimum has cluster
access for the target cluster.

**HelmRelease stalled after `OmniCluster` delete**

The controller's finalizer blocks CR deletion until all Omni resources are torn down. Omni
tears down nodes in order (MachineSetNodes → MachineSets → Cluster), which can take 1–2
minutes per node. Check controller logs for progress:

```bash
kubectl logs -n omni-gitops-system deploy/omni-gitops-controller -f
```

If Omni is unreachable and you need to force-remove the CR, manually patch out the finalizer:

```bash
kubectl patch omnicluster <name> -n omni-gitops-system \
  -p '{"metadata":{"finalizers":[]}}' --type=merge
```

This will leave Omni resources orphaned — clean them up manually in the Omni UI.

---

## Contributing

1. Fork the repository and clone it locally.
2. Build and verify: `go build ./...` and `go vet ./...` (requires Go 1.22+).
3. Make your changes. Keep diffs surgical — touch only what the change requires.
4. Open a pull request against `main` on [GitHub Issues](https://github.com/cgoolsby/omni-gitops-controller/issues).

Note: the Helm chart's `version` and `appVersion` are kept identical and are
bumped together to the tag version by the release workflow — don't bump them
manually in PRs.

---

## License

Apache 2.0 — see [LICENSE](LICENSE).
