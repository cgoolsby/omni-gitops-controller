# Prompt 02 — Pin golangci-lint and controller-gen Versions in CI

## Context

`.github/workflows/ci.yml` installs both lint/codegen tools with `@latest`:

```yaml
run: go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest
run: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
```

A new upstream release can break CI on a day when nothing in the project changed
(new linters, changed defaults, CRD generation differences). CI results are not
reproducible.

Note: this module is on go 1.26 (`go.mod`). golangci-lint v1.x is too old for
this toolchain — a **v2.x** release is required, and v2 changed the module path
to `github.com/golangci/golangci-lint/v2/cmd/golangci-lint` and the config file
format (`.golangci.yml` needs `version: "2"`). Determine the latest golangci-lint
v2 version that runs cleanly against this repo and pin that.

## What to Do

### 1. Pin golangci-lint

Replace the `go install @latest` step in the `lint` job with the official action:

```yaml
- name: Run golangci-lint
  uses: golangci/golangci-lint-action@v8
  with:
    version: v2.<x>.<y>   # the version you verified locally
    args: --timeout=5m
```

(The action caches the binary and makes the version explicit. v8 of the action is
the one that supports golangci-lint v2.)

Verify locally first: install that exact version
(`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.<x>.<y>`)
and run `golangci-lint run --timeout=5m`. If the existing `.golangci.yml` is in
v1 format, migrate it (`golangci-lint migrate` does this automatically) and fix
any new findings — or, if a finding is noise, configure the linter rather than
suppressing inline.

### 2. Pin controller-gen

In the `crd-drift` job, replace `controller-gen@latest` with a pinned version:
run `go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest` locally,
note the resolved version (`controller-gen --version`), and hardcode it, e.g.
`@v0.19.0`.

## Verification

```bash
golangci-lint run --timeout=5m         # passes with the pinned version
controller-gen --version               # matches the pinned version
python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/ci.yml'))"
```

## Constraints

- Do NOT commit. Do NOT touch `metaPrompts/` or unrelated files.

## Acceptance Criteria

- No `@latest` remains in `.github/workflows/ci.yml`.
- `gofmt -l .` clean; `go vet ./...`, `go build ./...`, `go test ./...` pass.
- The pinned golangci-lint runs clean against the repo locally.
