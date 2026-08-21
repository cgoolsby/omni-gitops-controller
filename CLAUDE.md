# CLAUDE.md

Guidance for Claude Code (and humans) working in this repository.

## What this is

`omni-gitops-controller` is a Kubernetes operator that bridges self-hosted
[Siderolabs Omni](https://omni.siderolabs.com) with GitOps (Flux/Argo). You declare
an `OmniCluster` custom resource; the controller calls Omni's native COSI gRPC API to
provision the Talos cluster and writes its kubeconfig as a `Secret` for Flux to
target. See `README.md` for the full architecture and user-facing reference.

- Module: `github.com/cgoolsby/omni-gitops-controller` (Go 1.26+)
- Single binary, entrypoint `main.go`. API in `api/v1alpha1/`, reconcilers in
  `controllers/`.

## Architecture in one paragraph

`OmniClusterReconciler` is an orchestrator: it decides which machines a cluster needs
and expresses that by creating/updating/deleting first-class, namespaced `Machine`
objects — the Deployment → ReplicaSet → Pod shape. `MachineReconciler` owns
per-machine Omni state (bind/unbind, config patches, kernel args, reboot RPC,
teardown via the `machine.omni.gitops.dev/finalizer`). Each `Machine.metadata.name`
is the raw Omni machine UUID; every `Machine` is owner-referenced by its
`OmniCluster` and watched via `.Owns(&Machine{})` (event-driven, no polling). The
controller holds no state of its own — Omni is authoritative, desired state lives in
the CR, runtime state in status subresources.

Key files:
- `controllers/omnicluster_controller.go` — orchestration, scale, reboot-candidate selection
- `controllers/machine_controller.go` — per-machine lifecycle
- `controllers/omni_client.go` — COSI gRPC calls into Omni
- `api/v1alpha1/types.go` — `OmniCluster`, `MachineSetSpec`, `WorkerMachineSetSpec`
- `api/v1alpha1/machine_types.go` — `Machine`

## Commands

```bash
make test        # go test -race ./...  (the primary gate)
make build       # build the binary into bin/
make manifests   # regenerate CRDs from +kubebuilder markers (see caveat below)
make generate    # regenerate DeepCopy methods
make fmt vet     # gofmt + go vet
make lint        # golangci-lint
```

Run `make test` after any change to controllers or API types.

## RBAC — two surfaces, keep them in sync

RBAC is maintained **by hand** in TWO places that must match:

1. `charts/omni-gitops-controller/templates/clusterrole.yaml` — this is what fixes
   real deployments (Helm is the recommended install).
2. `config/rbac/clusterrole.yaml` — the kustomize path.

**`make manifests` does NOT generate RBAC.** It only runs `controller-gen crd:...`
for CRDs (into `config/crd/bases`, then copies them to `charts/.../crds/`). There are
no `+kubebuilder:rbac` markers for events/leases/etc. — so editing a marker will not
update the ClusterRoles. Edit both YAML files directly.

### The events gotcha (v0.3.1)

The reconcilers record Events via a client-go `events.EventRecorder`
(`mgr.GetEventRecorder(...)` in `main.go`), which writes to the **`events.k8s.io`**
API group — not the legacy core (`""`) group. The ClusterRole must grant events in
BOTH groups (keep the core rule too; some clients still emit there):

```yaml
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create", "patch"]
- apiGroups: ["events.k8s.io"]
  resources: ["events"]
  verbs: ["create", "patch"]
```

Without the `events.k8s.io` rule every event write is denied
(`events.events.k8s.io is forbidden`) while reconciliation still succeeds — silent in
tests, visible only as ERROR logs in a real cluster.

## Releases

- Tag `vX.Y.Z` → `.github/workflows/release.yml` builds/pushes the image, cosign-signs
  it, attaches an SBOM, packages+pushes the Helm chart, and cuts a GitHub Release.
- `Chart.yaml` `version`/`appVersion` carry a real per-release semver in the tree, and
  CI (`release.yml`) re-stamps both from the tag at package time. Bump them to the
  version you are about to tag **before** tagging: consumers that build the chart from
  the git tree (below) dedup by chart version, so a stale in-tree `version` makes them
  serve a cached chart across releases even though the templates changed.
- Downstream consumers may pin the chart via a Flux `GitRepository` at a `ref.tag` and
  build the chart straight from the git tree — so a chart-template fix propagates via
  the tag, but consumers must move their `ref.tag` to pick it up.

## Conventions

- Follow existing style; match surrounding code. Keep changes minimal and focused.
- Don't change Go runtime behavior for an RBAC/chart/docs task, and vice versa.
- Unit tests tolerate a nil `Recorder` (see `eventf` in both controllers) — keep that.
