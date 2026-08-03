# Prompt 01 — Add `go test` to CI

## Context

The project has a real test suite in `controllers/omnicluster_controller_test.go`
(20+ test functions covering config patches, reboot gating, scale-down, teardown,
ownership, and more). However, `.github/workflows/ci.yml` only runs `go build`,
`go vet`, a CRD drift check, and `golangci-lint`. The tests are **never executed
in CI** — a regression that breaks any of them would ship silently.

## What to Do

Edit `.github/workflows/ci.yml` and add a new job called `test` that runs in
parallel with `build`, `crd-drift`, and `lint`:

- `actions/checkout@v6`
- `actions/setup-go@v6` with `go-version-file: go.mod` and `cache: true`
- `go test -v -race ./...`

The `-race` flag matters because controller-runtime uses goroutines internally.

## Verification

```bash
go test -v -race ./...   # must pass locally
python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/ci.yml'))"
```

## Constraints

- Do NOT commit. Do NOT touch `metaPrompts/` or unrelated files.

## Acceptance Criteria

- `.github/workflows/ci.yml` contains a `test` job running `go test -v -race ./...`.
- `gofmt -l .` clean; `go vet ./...`, `go build ./...`, `go test ./...` pass.
