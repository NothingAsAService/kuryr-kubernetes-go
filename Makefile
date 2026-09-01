# Kuryr-Kubernetes (Go) — kubebuilder v4 Makefile.
#
# Kuryr ships three binaries rather than the single manager kubebuilder
# scaffolds, so the build and docker targets are parameterised by binary.
# Everything else is the stock kubebuilder target.

# Image tags. Override on the command line: make docker-build TAG=v1.2.3
REGISTRY ?= quay.io/kuryr
TAG ?= latest
CONTROLLER_IMG ?= $(REGISTRY)/controller:$(TAG)
CNI_IMG ?= $(REGISTRY)/cni:$(TAG)

# ENVTEST_K8S_VERSION is the control plane the envtest suites run against.
ENVTEST_K8S_VERSION = 1.37.0

ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

CONTAINER_TOOL ?= docker

# Fail a recipe on the first failing command, not the last.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Development

# controller-gen exits non-zero on a directory that has no .go files of its
# own, so the paths below name the packages that actually hold code rather
# than the trees above them ("./pkg/..." fails before writing anything).
# Add a package here when it grows its first kubebuilder marker.
.PHONY: manifests
manifests: controller-gen ## Generate the CRDs and the ClusterRole from the kubebuilder markers.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd \
		paths="./api/v1/...;./internal/controller/...;./pkg/cni/watcher/..." \
		output:crd:artifacts:config=config/crd/bases \
		output:rbac:artifacts:config=config/rbac

.PHONY: generate
generate: controller-gen ## Generate the DeepCopy methods.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/v1/..."

.PHONY: fmt
fmt: ## Run go fmt against the code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against the code.
	go vet ./...
	# The CNI node agent has a Linux-only half that is excluded from the
	# Windows build, so a developer on another platform never compiles it.
	# Vetting for Linux explicitly is what keeps that half honest.
	GOOS=linux GOARCH=amd64 go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run the unit and envtest suites.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		go test $$(go list ./... | grep -v /test/e2e) -coverprofile cover.out

.PHONY: test-race
test-race: setup-envtest ## Run the suites under the race detector. Needs cgo.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		go test -race $$(go list ./... | grep -v /test/e2e)

.PHONY: test-e2e
test-e2e: ## Run the e2e suite against an existing Kind cluster.
	@$(KIND) get clusters | grep -q 'kuryr-e2e' || { \
		echo "no kind cluster named kuryr-e2e; run 'make setup-test-e2e' first"; exit 1; }
	go test ./test/e2e/ -v

.PHONY: setup-test-e2e
setup-test-e2e: kind ## Create the Kind cluster the e2e suite runs against.
	$(KIND) create cluster --name kuryr-e2e

.PHONY: cleanup-test-e2e
cleanup-test-e2e: kind ## Delete that Kind cluster.
	$(KIND) delete cluster --name kuryr-e2e

.PHONY: lint
lint: golangci-lint ## Run golangci-lint.
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint and apply what it can fix itself.
	$(GOLANGCI_LINT) run --fix

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build all three binaries into bin/.
	go build -o bin/kuryr-controller ./cmd/kuryr-controller
	go build -o bin/kuryr-daemon ./cmd/kuryr-daemon
	go build -o bin/kuryr-cni ./cmd/kuryr-cni

KURYR_CONF ?= etc/kuryr.conf

.PHONY: run
run: manifests generate fmt vet ## Run the controller against the cluster in ~/.kube/config.
	go run ./cmd/kuryr-controller --config-file=$(KURYR_CONF)

.PHONY: docker-build
docker-build: ## Build both container images.
	$(CONTAINER_TOOL) build -f build/controller.Dockerfile -t $(CONTROLLER_IMG) .
	$(CONTAINER_TOOL) build -f build/cni.Dockerfile -t $(CNI_IMG) .

.PHONY: docker-push
docker-push: ## Push both container images.
	$(CONTAINER_TOOL) push $(CONTROLLER_IMG)
	$(CONTAINER_TOOL) push $(CNI_IMG)

.PHONY: build-installer
build-installer: manifests generate kustomize ## Render the whole deployment into dist/install.yaml.
	mkdir -p dist
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(CONTROLLER_IMG)
	cd config/cni && $(KUSTOMIZE) edit set image cni=$(CNI_IMG)
	$(KUSTOMIZE) build config/default > dist/install.yaml

# HELM_CHART_DIR is both the output directory and, through its last path
# element, the chart name helmify writes into Chart.yaml. Renaming it renames
# every resource the chart creates.
#
# The chart directory is removed first: helmify only ever adds to a chart, so
# a template for a resource that no longer exists would survive a
# regeneration and go on being applied.
#
# helmify itself cannot express two service accounts, so the chart it writes
# hands the privileged node agent the controller's permissions. The script the
# recipe runs afterwards puts that split back; it is not optional.
HELM_CHART_DIR ?= dist/kuryr

.PHONY: helm
helm: manifests generate kustomize helmify ## Render the whole deployment as a Helm chart into dist/kuryr.
	mkdir -p dist
	rm -rf $(HELM_CHART_DIR)
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(CONTROLLER_IMG)
	cd config/cni && $(KUSTOMIZE) edit set image cni=$(CNI_IMG)
	$(KUSTOMIZE) build config/default | $(HELMIFY) $(HELM_CHART_DIR)
	./hack/helm-split-serviceaccounts.sh $(HELM_CHART_DIR)

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install the CRDs into the cluster.
	$(KUSTOMIZE) build config/crd | kubectl apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Remove the CRDs from the cluster.
	$(KUSTOMIZE) build config/crd | kubectl delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy the controller and the CNI agent.
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(CONTROLLER_IMG)
	cd config/cni && $(KUSTOMIZE) edit set image cni=$(CNI_IMG)
	$(KUSTOMIZE) build config/default | kubectl apply -f -

.PHONY: undeploy
undeploy: kustomize ## Remove them again.
	$(KUSTOMIZE) build config/default | kubectl delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint
HELMIFY ?= $(LOCALBIN)/helmify
KIND ?= $(LOCALBIN)/kind

# Pinned, so a build is reproducible and an upgrade is a reviewable diff
# rather than something that happens on its own the next time CI runs.
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.21.0
ENVTEST_VERSION ?= release-0.24
GOLANGCI_LINT_VERSION ?= v2.13.2
HELMIFY_VERSION ?= v0.4.20
KIND_VERSION ?= v0.33.0

.PHONY: kustomize
kustomize: $(KUSTOMIZE)
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN)
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: $(ENVTEST)
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT)
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

.PHONY: kind
kind: $(KIND)
$(KIND): $(LOCALBIN)
	$(call go-install-tool,$(KIND),sigs.k8s.io/kind,$(KIND_VERSION))

.PHONY: helmify
helmify: $(HELMIFY)
$(HELMIFY): $(LOCALBIN)
	$(call go-install-tool,$(HELMIFY),github.com/arttor/helmify/cmd/helmify,$(HELMIFY_VERSION))

# go-install-tool installs a tool at a pinned version into $(LOCALBIN),
# keeping the version in the filename so switching branches reinstalls rather
# than silently reusing the wrong binary.
# $1 - target path, $2 - package url, $3 - version
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
