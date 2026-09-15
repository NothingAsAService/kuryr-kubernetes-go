# Kuryr-Kubernetes (Go) — kubebuilder v4 Makefile.
#
# Kuryr ships four binaries rather than the single manager kubebuilder
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

.PHONY: check
check: test lint ## Everything CI runs: generate, vet, test and lint.

# A generated file that is out of date with its source is a silent
# divergence: the cluster gets the CRD from config/crd, not from the markers,
# so a schema change that was never regenerated is only noticed when an object
# is rejected. This is the target to run in CI, after `make manifests
# generate`.
.PHONY: verify-generated
verify-generated: manifests generate ## Fail if the generated files are not committed.
	@if ! git diff --quiet -- config/crd api/v1/zz_generated.deepcopy.go config/rbac; then \
		echo "generated files are out of date; run 'make manifests generate' and commit the result:"; \
		git --no-pager diff --stat -- config/crd api/v1/zz_generated.deepcopy.go config/rbac; \
		exit 1; \
	fi

##@ Build

BINARIES = kuryr-controller kuryr-daemon kuryr-cni kuryr-status

.PHONY: build
build: manifests generate fmt vet ## Build all four binaries into bin/, for this machine.
	@for binary in $(BINARIES); do \
		echo "building bin/$$binary"; \
		go build -o bin/$$binary ./cmd/$$binary || exit 1; \
	done

# The shipped binaries are Linux binaries: kuryr-daemon and kuryr-cni drive
# netlink and network namespaces, and on any other platform they compile
# against stubs that refuse every call. `build` above still builds for the
# host, which is what `go run` and a debugger need; this is what you copy to a
# node.
TARGETARCH ?= amd64
DIST_DIR = bin/linux_$(TARGETARCH)

.PHONY: build-linux
build-linux: manifests generate ## Cross-compile all four binaries for linux into bin/linux_<arch>/.
	@mkdir -p $(DIST_DIR)
	@for binary in $(BINARIES); do \
		echo "building $(DIST_DIR)/$$binary"; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$(TARGETARCH) \
			go build -trimpath -ldflags="-s -w" -o $(DIST_DIR)/$$binary ./cmd/$$binary || exit 1; \
	done

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

# Kind reads images from its own containerd, not from the host daemon, so an
# image built locally is invisible to it until it is loaded. This is what
# makes a build-deploy-test round trip possible without a registry at all.
.PHONY: kind-load
kind-load: kind ## Load both locally built images into the kind cluster.
	$(KIND) load docker-image $(CONTROLLER_IMG) --name kuryr-e2e
	$(KIND) load docker-image $(CNI_IMG) --name kuryr-e2e

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
	# Run through bash rather than relying on the shebang. Resolving a
	# shebang from a recipe goes through the Windows working directory, which
	# a space in the path then splits -- and this step is the one that must
	# not be skipped, so it must not be the one that silently cannot run.
	bash ./hack/helm-split-serviceaccounts.sh $(HELM_CHART_DIR)

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

# Tool binaries land here.
#
# GNU make cannot express a target whose name contains a space, so a checkout
# under such a path -- "C:/Users/me/My Documents/kuryr-go", a synced OneDrive
# or Dropbox folder -- would break every tool target with a stream of
# "overriding recipe" warnings and no working tools. Rather than leave that as
# a trap with a note beside it, fall back to a path that has no space.
empty :=
space := $(empty) $(empty)
LOCALBIN ?= $(shell pwd)/bin
ifneq (,$(findstring $(space),$(LOCALBIN)))
# `go env GOPATH` answers in the native path syntax, which on Windows means
# backslashes -- and the recipe shell is sh, which would eat them. Normalise
# before using it anywhere.
LOCALBIN := $(subst \,/,$(shell go env GOPATH))/bin/kuryr-tools
$(info this checkout is under a path containing a space, so tools go to $(LOCALBIN) instead of ./bin)
endif

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

# Go appends .exe on Windows. Without this every tool target reinstalls on
# every run, because the file it looks for is never the one go install wrote.
ifeq ($(shell go env GOOS),windows)
EXE := .exe
endif

KUSTOMIZE ?= $(LOCALBIN)/kustomize$(EXE)
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen$(EXE)
ENVTEST ?= $(LOCALBIN)/setup-envtest$(EXE)
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint$(EXE)
HELMIFY ?= $(LOCALBIN)/helmify$(EXE)
KIND ?= $(LOCALBIN)/kind$(EXE)

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
	$(call go-install-tool,kustomize,sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN)
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,controller-gen,sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: $(ENVTEST)
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,setup-envtest,sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT)
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,golangci-lint,github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

.PHONY: kind
kind: $(KIND)
$(KIND): $(LOCALBIN)
	$(call go-install-tool,kind,sigs.k8s.io/kind,$(KIND_VERSION))

.PHONY: helmify
helmify: $(HELMIFY)
$(HELMIFY): $(LOCALBIN)
	$(call go-install-tool,helmify,github.com/arttor/helmify/cmd/helmify,$(HELMIFY_VERSION))

# go-install-tool installs a tool at a pinned version into $(LOCALBIN),
# keeping the version in the filename so switching branches reinstalls rather
# than silently reusing the wrong binary. The unversioned name is a copy, not
# a symlink: Windows does not give an ordinary user the right to create one.
#
# $1 - tool name, $2 - package url, $3 - version
define go-install-tool
@[ -f "$(LOCALBIN)/$(1)-$(3)$(EXE)" ] || { \
set -e ;\
echo "Downloading $(2)@$(3)" ;\
rm -f "$(LOCALBIN)/$(1)$(EXE)" ;\
GOBIN="$(LOCALBIN)" go install $(2)@$(3) ;\
mv "$(LOCALBIN)/$(1)$(EXE)" "$(LOCALBIN)/$(1)-$(3)$(EXE)" ;\
} ;\
cp -f "$(LOCALBIN)/$(1)-$(3)$(EXE)" "$(LOCALBIN)/$(1)$(EXE)"
endef
