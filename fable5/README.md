# fable5 — Lifecycle & Reboot-Safety Fix Series

Ordered prompt series produced from a code review of the controller's node-lifecycle
handling (reboots, scale-down, teardown, status convergence). Each prompt is
self-contained and designed to be executed by a headless Claude session.

## Running

```bash
./fable5/run.sh
```

The script:

1. Creates (or resumes) the branch `fable5/lifecycle-review-fixes`.
2. Executes each `NN-*.md` prompt in order via `claude -p`.
3. After each prompt, gates on `gofmt` / `go vet` / `go build` / `go test`.
   On failure it gives Claude one retry with the failure output, then aborts.
4. Commits each prompt's changes individually (prompts never commit themselves).
5. Pushes the branch and opens a PR with `gh pr create`.

Already-completed prompts (detected via commit messages) are skipped, so the
script is safe to re-run after a failure.

Environment overrides:

| Variable | Default | Purpose |
|---|---|---|
| `FABLE5_BRANCH` | `fable5/lifecycle-review-fixes` | Branch to work on |
| `CLAUDE_BIN` | `claude` | Claude CLI binary |
| `FABLE5_MODEL` | (account default) | `--model` override |
| `FABLE5_YOLO` | unset | Set to `1` to use `--dangerously-skip-permissions` instead of the curated allowlist |

## Prompt order

| # | Prompt | Theme |
|---|---|---|
| 01 | reboot-health-gate | Never reboot while any machine in the cluster is unhealthy |
| 02 | reboot-cooldown | Per-machine reboot cooldown; no infinite reboot loops |
| 03 | cp-scaledown-one-at-a-time | Control-plane scale-down removes one member per reconcile |
| 04 | status-convergence | Stop the status-update hot loop; converging conditions |
| 05 | kubeconfig-refresh-throttle | Stop minting a new Omni SA token every reconcile |
| 06 | teardown-from-omni-state | Finalizer teardown lists Omni state instead of trusting status |
| 07 | evicted-machine-cleanup | Evicted machines release their KernelArgs |
| 08 | crd-validation | CRD-level validation (replicas, versions, matchExpressions) |
| 09 | cluster-ownership-guard | Prevent two OmniClusters from adopting the same Omni cluster |
| 10 | machinesetnode-conflict-guard | Don't silently claim machines bound to another set |
