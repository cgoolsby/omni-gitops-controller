# fable6 — metaPrompts Backlog Series

Executes the user's `metaPrompts/` backlog (triaged and revised against the
fable5 lifecycle changes) as an ordered prompt series. Runs on branch
`fable6/metaprompts-backlog`, stacked on `fable5/lifecycle-review-fixes`
(PR #20); the resulting PR targets that branch.

```bash
./fable6/run.sh
```

Mechanics (gating, per-prompt commits, resume, PR creation) are shared with
`fable5/run.sh` — see `fable5/README.md`.

## Prompt order

| # | Source metaPrompt | Theme |
|---|---|---|
| 01 | 01 | `go test -race` job in CI |
| 02 | 12 | Pin golangci-lint (v2.x) + controller-gen in CI |
| 03 | 11 | Makefile; CI drift check via `make manifests` (covers chart CRD copy) |
| 04 | 13 | Remove nested planning-docs directory |
| 05 | 16 | Align chart version/appVersion |
| 06 | 06 | Prune orphaned worker machine sets (Omni-as-truth, full cleanup) |
| 07 | 05 | Implement matchExpressions; drop CEL rejection |
| 08 | 10 | Top-level `status.observedGeneration` |
| 09 | 08 | Kubernetes Events |
| 10 | 09 | Custom Prometheus metrics |
| 11 | 14 | Helm metrics Service + ServiceMonitor |
| 12 | 15 | Cosign keyless signing + SBOM |
| 13 | 02 | README/example: machineExtensions + kernelArgs |
| 14 | 03 | README: config drift & safe rolling reboot (post-fable5 behaviour) |
| 15 | 07 | README: accurate conditions/status-fields table |

Not included: metaPrompt 04 (kubeconfig Secret deletion) — already implemented
in commit `b3a158a`.

Docs prompts run last so they document final behaviour.
