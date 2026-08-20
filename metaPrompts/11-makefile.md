# Prompt 11 — Add a Makefile

## Context

The project has no `Makefile`. Contributors and maintainers must read `.github/workflows/ci.yml` to discover the correct commands for building, testing, linting, and regenerating generated files. Standard kubebuilder-based projects ship a `Makefile` with well-known targets, and this project would benefit from the same convention.

---

## What to Do

Create a `Makefile` at the repo root with at minimum these targets:

```makefile
# Tool versions
GOLANGCI_LINT_VERSION ?= v1.62.0
CONTROLLER_GEN_VERSION ?= latest

# Binary output
BINARY ?= bin/omni-gitops-controller

.PHONY: all build test lint vet fmt manifests generate tidy docker-build help

all: build

## build: Compile the controller binary
build:
	go build -o $(BINARY) ./...

## test: Run the test suite with the race detector
test:
	go test -v -race ./...

## vet: Run go vet
vet:
	go vet ./...

## fmt: Run gofmt
fmt:
	gofmt -s -w .

## lint: Run golangci-lint (installs if not present)
lint:
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	golangci-lint run --timeout=5m

## manifests: Regenerate CRD YAML from Go types
manifests:
	go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)
	controller-gen crd:generateEmbeddedObjectMeta=true \
	  paths="./api/..." \
	  output:crd:artifacts:config=config/crd/bases

## generate: Regenerate deepcopy methods
generate:
	go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)
	controller-gen object paths="./api/..."

## tidy: Run go mod tidy
tidy:
	go mod tidy

## docker-build: Build the Docker image
docker-build:
	docker build -t ghcr.io/cgoolsby/omni-gitops-controller:dev .

## help: Show this help message
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
```

Notes:
- `GOLANGCI_LINT_VERSION` should match the version pinned in `.github/workflows/ci.yml` (see prompt 12).
- Add `bin/` to `.gitignore`.
- The `manifests` and `generate` targets should be the single source of truth — the CI CRD drift check should ideally call `make manifests` instead of inlining the controller-gen command.

---

## Verification

```bash
make build    # should produce bin/omni-gitops-controller
make test     # should run and pass all tests
make vet      # should exit 0
make manifests && git diff --exit-code config/crd/bases/  # should be clean
```
