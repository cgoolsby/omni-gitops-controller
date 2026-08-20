# Prompt 01 — Add `go test` to CI

## Context

The project has a real test suite in `controllers/omnicluster_controller_test.go` covering:
- `EnsureConfigPatch` create/update/no-op behavior
- `applyConfigPatches` prune-on-remove
- `reconcileMachineSet` KernelArgs prune-on-clear
- `reconcileMachineSet` ExtensionsConfiguration prune-on-clear

However, `.github/workflows/ci.yml` only runs `go build`, `go vet`, a CRD drift check, and `golangci-lint`. The tests are **never executed in CI**. A regression that breaks any of these tests would ship silently.

---

## What to Do

Edit `.github/workflows/ci.yml` and add a `test` job (or a step inside the existing `build` job) that runs:

```bash
go test -v -race ./...
```

Recommended: add it as a new job called `test` that runs in parallel with `build`, `crd-drift`, and `lint`. It should:
- Use `actions/checkout@v6`
- Use `actions/setup-go@v6` with `go-version-file: go.mod` and `cache: true`
- Run `go test -v -race ./...`

The `-race` flag is important because the controller uses goroutines internally (controller-runtime). The `-v` flag makes test output readable in the GitHub Actions log.

---

## Verification

1. Open a pull request (or push directly) and confirm the new `test` job appears in the Actions tab.
2. Temporarily break one test (e.g., change an expected value in `TestEnsureConfigPatch_UpdateOnChange`) and confirm CI fails on the `test` job, not silently.
3. Revert the breakage and confirm CI is green.
