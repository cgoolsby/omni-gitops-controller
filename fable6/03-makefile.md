# Prompt 03 — Add a Makefile (Single Source of Truth for Dev Commands)

## Context

The project has no `Makefile`. Contributors must read `.github/workflows/ci.yml`
to discover the right commands for building, testing, linting, and regenerating
generated files. Standard kubebuilder-style projects ship a Makefile with
well-known targets.

## What to Do

Create a `Makefile` at the repo root with these targets (phony where
appropriate):

- `build` — `go build -o bin/omni-gitops-controller .`
- `test` — `go test -v -race ./...`
- `vet`, `fmt` (`gofmt -s -w .`), `tidy`
- `lint` — installs the **same pinned golangci-lint version as CI** (read the
  version from `.github/workflows/ci.yml` — prompt 02 pinned it; define it once
  as `GOLANGCI_LINT_VERSION ?=` at the top) and runs it
- `manifests` — installs the **pinned controller-gen version from CI**
  (`CONTROLLER_GEN_VERSION ?=`), then runs
  `controller-gen crd:generateEmbeddedObjectMeta=true paths="./api/..." output:crd:artifacts:config=config/crd/bases`
  **and copies the result to `charts/omni-gitops-controller/crds/`** (the chart
  copy must never drift)
- `generate` — `controller-gen object paths="./api/..."`
- `docker-build` — `docker build -t ghcr.io/cgoolsby/omni-gitops-controller:dev .`
- `help` — self-documenting from `## target:` comments

Also:

- Add `bin/` to `.gitignore` if not present.
- Update the `crd-drift` job in `.github/workflows/ci.yml` to run
  `make manifests` followed by
  `git diff --exit-code config/crd/bases/ charts/omni-gitops-controller/crds/`
  instead of inlining the controller-gen command — the Makefile becomes the
  single source of truth, and the drift check now also covers the chart copy.

## Verification

```bash
make build && make test && make vet
make manifests && git diff --exit-code config/crd/bases/ charts/omni-gitops-controller/crds/
make help
```

## Constraints

- Do NOT commit. Do NOT touch `metaPrompts/` or unrelated files.

## Acceptance Criteria

- All targets above work; tool versions are defined once and match CI.
- `gofmt -l .` clean; `go vet ./...`, `go build ./...`, `go test ./...` pass.
