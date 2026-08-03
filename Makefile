# Image URL to use for docker-build.
IMG ?= ghcr.io/cgoolsby/omni-gitops-controller:dev

# Tool versions — keep in sync with .github/workflows/ci.yml.
GOLANGCI_LINT_VERSION ?= v2.12.2
CONTROLLER_GEN_VERSION ?= v0.21.0

# Location to install tool binaries.
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate CRD manifests and sync the chart copy.
	$(CONTROLLER_GEN) crd:generateEmbeddedObjectMeta=true paths="./api/..." output:crd:artifacts:config=config/crd/bases
	cp config/crd/bases/*.yaml charts/omni-gitops-controller/crds/

.PHONY: generate
generate: controller-gen ## Generate DeepCopy methods for API types.
	$(CONTROLLER_GEN) object paths="./api/..."

.PHONY: fmt
fmt: ## Run gofmt against code.
	gofmt -s -w .

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: tidy
tidy: ## Run go mod tidy.
	go mod tidy

.PHONY: test
test: ## Run tests with the race detector.
	go test -v -race ./...

.PHONY: lint
lint: golangci-lint ## Run golangci-lint.
	$(GOLANGCI_LINT) run --timeout=5m

##@ Build

.PHONY: build
build: ## Build the controller binary into bin/.
	go build -o bin/omni-gitops-controller .

.PHONY: docker-build
docker-build: ## Build the docker image.
	docker build -t $(IMG) .

##@ Tooling

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Install controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_GEN_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Install golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

# go-install-tool installs $(2)@$(3) into $(1)-$(3) and symlinks $(1) to it,
# so bumping a *_VERSION variable triggers a reinstall.
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef
