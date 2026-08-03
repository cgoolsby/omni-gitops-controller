# Prompt 04 — Converging Status Updates (Fix the Hot Loop)

## Context

`setCondition` in `controllers/omnicluster_controller.go` stamps
`LastTransitionTime: metav1.Now()` on every call, so the status differs on every
reconcile, so `r.Status().Update(...)` always writes. The controller watches its
own CR (`For(&api.OmniCluster{})`), and a status write fires a watch event, which
immediately retriggers reconcile. The intended `RequeueAfter` polling cadence
(15s/60s) is therefore fiction — the controller effectively reconciles in a tight
loop, hammering the Omni API.

## What to Do

In `controllers/omnicluster_controller.go`:

1. Replace the hand-rolled `setCondition` helper with
   `meta.SetStatusCondition(&cluster.Status.Conditions, cond)` from
   `k8s.io/apimachinery/pkg/api/meta`. Build conditions **without** setting
   `LastTransitionTime` — `meta.SetStatusCondition` preserves the existing
   transition time when the condition status is unchanged and stamps a new one
   only on actual transitions. Set `ObservedGeneration: cluster.Generation` on
   each condition while you're here.
2. Make status updates conditional: capture
   `statusBefore := cluster.Status.DeepCopy()` immediately after fetching the CR,
   and at update time only call `r.Status().Update(ctx, cluster)` when
   `!equality.Semantic.DeepEqual(statusBefore, &cluster.Status)`
   (`k8s.io/apimachinery/pkg/api/equality`).
3. Apply the same two changes to `setFailure` (it also writes conditions and
   status unconditionally).
4. Delete the now-unused `setCondition` helper. Keep `boolToConditionStatus` if
   still used, otherwise delete it too.

Note: `meta.SetStatusCondition` requires a non-empty `Reason` matching the
standard condition reason format. The existing reasons (Omni phase strings,
`PendingReboot`, etc.) are valid; just be aware if you introduce new ones.

## Tests

The existing test suite must keep passing. If practical, add a focused unit test
for the conditional-update decision (e.g. a helper that takes before/after status
and reports whether an update is needed) — but do not introduce envtest or a fake
API server for this; keep it lightweight.

## Constraints

- Do NOT commit. Do NOT touch `metaPrompts/` or unrelated files.

## Acceptance Criteria

- `gofmt -l .` clean; `go vet ./...`, `go build ./...`, `go test ./...` pass.
- No code path stamps `LastTransitionTime` manually anymore.
- `r.Status().Update` is only reachable when the status actually changed.
