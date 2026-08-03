# Prompt 04 — Remove Internal Planning Docs Directory

## Context

The directory `omni-gitops-controller/` at the repo root (confusingly named the
same as the repo) contains 12 internal implementation-planning markdown files
(`00-create-github-repo.md` … `10-config-drift-reboot.md`, `PLAN.md`) used during
initial development. They are not user-facing docs, not API reference, and not
contributing guidelines — just noise for anyone browsing the project. The
architecture reasoning worth keeping ("Why Not CAPI?") is already in the README.

## What to Do

```bash
git rm -r omni-gitops-controller/
```

Do not create a `docs/` directory or `PLANNING.md` replacement — the work is
done and the code is the artifact.

## Verification

```bash
ls omni-gitops-controller/   # No such file or directory
go build ./...               # nothing referenced those files
```

## Constraints

- Only delete `omni-gitops-controller/` — nothing else. Do NOT touch
  `metaPrompts/` or `fable5/`/`fable6/`.
- Do NOT commit (the runner commits; `git rm` staging is fine).

## Acceptance Criteria

- The directory is gone; `gofmt -l .` clean; `go vet ./...`, `go build ./...`,
  `go test ./...` pass.
