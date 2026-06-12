# Prompt 05 — Stop Minting a New Omni Service-Account Token Every Reconcile

## Context

`GetKubeconfig` (`controllers/omni_client.go`) calls
`management.WithServiceAccount(90*24*time.Hour, "flux-admin", "system:masters")`
unconditionally, and `ensureKubeconfigSecret` / `ensureArgoCDClusterSecret`
(`controllers/omnicluster_controller.go`) run on **every** reconcile of a ready
cluster. That mints a fresh 90-day service-account kubeconfig from Omni every
cycle — wasteful at the intended 60s cadence and worse before the hot-loop fix.

## What to Do

### 1. Add a refresh decision helper

In `controllers/omnicluster_controller.go` add:

```go
const (
    kubeconfigRefreshedAtAnnotation = "omni.gitops.dev/kubeconfig-refreshed-at"
    // kubeconfigRefreshInterval is how often the kubeconfig Secret is re-fetched
    // from Omni. Must be comfortably shorter than the 90-day token TTL requested
    // in GetKubeconfig so the token never expires in place.
    kubeconfigRefreshInterval = 30 * 24 * time.Hour
)
```

and a pure helper:

```go
// secretNeedsRefresh reports whether the kubeconfig Secret must be re-fetched
// from Omni: it is missing, lacks the expected data key, or its refresh
// annotation is absent, unparsable, or older than the refresh interval.
func secretNeedsRefresh(secret *corev1.Secret, dataKey string, now time.Time) bool
```

### 2. Use it in both ensure functions

In `ensureKubeconfigSecret` and `ensureArgoCDClusterSecret`:

- First `r.Get` the Secret (use `client.ObjectKey{Namespace: r.KubeconfigNamespace, Name: ...}`).
  Treat NotFound as "needs refresh"; propagate other errors.
- If the Secret exists and `secretNeedsRefresh` returns false, return nil
  **without** calling `GetKubeconfig`.
- When writing the Secret, set the
  `omni.gitops.dev/kubeconfig-refreshed-at` annotation to `now` in RFC3339
  format inside the `CreateOrUpdate` mutate function.
- Data keys: `"value"` for the Flux secret, `"config"` for the ArgoCD secret.

### 3. Update the stale comment

The doc comment on `GetKubeconfig` claims "ensureKubeconfigSecret refreshes it on
every reconcile" — update it to describe the new 30-day refresh behaviour.

## Tests

Unit-test `secretNeedsRefresh` in `controllers/omnicluster_controller_test.go`:

- nil Secret → true.
- Secret without the data key → true.
- Secret with data but no annotation → true.
- Annotation 1 hour old → false.
- Annotation older than the interval → true.
- Unparsable annotation → true.

## Constraints

- Do NOT commit. Do NOT touch `metaPrompts/` or unrelated files.

## Acceptance Criteria

- `gofmt -l .` clean; `go vet ./...`, `go build ./...`, `go test ./...` pass.
- A ready cluster with a fresh Secret performs zero `GetKubeconfig` calls.
