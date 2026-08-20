# Prompt 12 — Pin golangci-lint Version in CI

## Context

`.github/workflows/ci.yml` installs golangci-lint with:

```yaml
- name: Install golangci-lint
  run: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
```

Using `@latest` means the installed version changes whenever golangci-lint publishes a new release. This can cause CI to fail on a day when nothing in the project changed — a new golangci-lint version may introduce new linters, change defaults, or alter existing behavior.

---

## What to Do

### 1. Pin the version in CI

Replace the `go install @latest` approach with the official golangci-lint install action, which is faster and supports pinning:

```yaml
- name: Install golangci-lint
  uses: golangci/golangci-lint-action@v6
  with:
    version: v1.62.0
    args: --timeout=5m
```

Using the action instead of `go install` is preferred because:
- It caches the binary between runs.
- It uses the official install script (not `go install`, which rebuilds from source).
- The version is explicit and human-readable in the YAML.

If you prefer not to use the action, the alternative is:

```yaml
- name: Install golangci-lint
  run: |
    curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \
      | sh -s -- -b $(go env GOPATH)/bin v1.62.0
```

### 2. Keep the Makefile in sync

If prompt 11 (Makefile) is implemented, update `GOLANGCI_LINT_VERSION` in the Makefile to match the version pinned in CI.

### 3. Update policy

When upgrading golangci-lint, do it as an intentional, standalone commit with a message like:
```
chore(ci): upgrade golangci-lint to v1.63.0
```
This separates linter upgrades from feature/fix commits, making regressions easy to bisect.

---

## Verification

After the change:
1. Open a PR that only changes the golangci-lint version line.
2. Confirm CI passes and shows the pinned version in the step output.
3. Confirm the version doesn't change across two CI runs with no code changes.
