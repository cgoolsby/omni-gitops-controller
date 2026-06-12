# Prompt 09 — Guard Against Two OmniClusters Adopting the Same Omni Cluster

## Context

The Omni cluster ID is just `cluster.Name` (`OmniCluster` is namespaced).
`team-a/prod` and `team-b/prod` therefore silently adopt and fight over the same
Omni cluster — and deleting either CR destroys it. `EnsureCluster`
(`controllers/omni_client.go`) happily adopts any existing Omni cluster with a
matching name.

## What to Do

### 1. Stamp and check an owner label

In `controllers/omni_client.go`:

- Add a constant `ownerLabel = "omni.gitops.dev/owner"`.
- Change `EnsureCluster` to take an `owner string` parameter (the caller passes
  `<namespace>/<name>` of the OmniCluster CR).
- On **create**: set the `ownerLabel` to `owner` on the new Cluster resource's
  metadata labels.
- On **adopt** (cluster already exists):
  - If the existing cluster's `ownerLabel` matches `owner` → proceed as today.
  - If the label is **absent** (cluster pre-dates this controller or was created
    by hand) → adopt it: set the label and update the resource. Document this
    adopt-and-stamp behaviour in the doc comment.
  - If the label is present and **differs** → return a distinct error, e.g.
    `fmt.Errorf("omni cluster %q is owned by %s, refusing to adopt (this OmniCluster is %s)", ...)`.
    Note that the existing version/spec update must also only happen for the
    matching/adopted owner.

### 2. Surface the conflict in the reconciler

In `controllers/omnicluster_controller.go`, pass
`cluster.Namespace + "/" + cluster.Name` to `EnsureCluster`. The existing
`setFailure(..., "EnsureClusterFailed", err)` path already surfaces the error;
optionally use a dedicated reason `ClusterOwnershipConflict` when the ownership
error is returned (a sentinel error with `errors.Is`, or a typed error, makes
this clean).

Also consider the **delete path**: `deleteOmniResources` should not destroy an
Omni cluster owned by a different CR. Fetch the cluster first and skip the
destroy (log a warning) if the owner label exists and differs.

## Tests

In `controllers/omnicluster_controller_test.go`:

- Create via `EnsureCluster` with owner `ns-a/prod`; call again with the same
  owner → no error, versions update as before.
- Call `EnsureCluster` for the same name with owner `ns-b/prod` → error.
- Pre-create a Cluster resource with no owner label directly in the state; call
  `EnsureCluster` → adopted, label now set.

## Constraints

- Do NOT commit. Do NOT touch `metaPrompts/` or unrelated files.

## Acceptance Criteria

- `gofmt -l .` clean; `go vet ./...`, `go build ./...`, `go test ./...` pass.
