# orama-operator — standard kubebuilder targets.
# Requires: go on PATH (export PATH="/opt/homebrew/bin:$(go env GOPATH)/bin:$PATH");
# controller-gen is fetched on demand by `make generate`/`make manifests`.

IMG ?= oramasearch/orama-operator:latest
CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.21.0

.PHONY: manifests generate fmt vet build run docker-build helm-package test check-crds verify-workspace-isolation

manifests: ## Generate CRDs + RBAC from kubebuilder markers into config/crd, config/rbac.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook \
		paths="./..." output:crd:artifacts:config=config/crd/bases

generate: ## Generate DeepCopy methods.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

fmt: ; go fmt ./...
vet: ; go vet ./...
build: generate fmt vet ; go build -o bin/manager cmd/main.go
run: ## Run the manager locally (out-of-cluster) against your current kube-context, without the deployment listener.
	go run ./cmd --control-plane-bind-address=""
test: ; go test ./... -coverprofile cover.out
docker-build: ; docker build -t $(IMG) .

helm-package: manifests ## Copy generated CRDs into the chart and package it.
	cp config/crd/bases/*.yaml chart/crds/
	helm package chart -d dist/

check-crds: ## Fail if the cluster is missing a CRD this chart ships. CONTEXT=<kube-context>
	./hack/check-cluster-crds.sh $(CONTEXT)

verify-workspace-isolation: ## Run the workspace trust-boundary Job against a live cluster.
	@test -n "$(WORKSPACE_NAMESPACE)" -a -n "$(LAKE_NAMESPACE)" -a -n "$(LAKE_NAME)" -a -n "$(OTHER_WORKSPACE_NAMESPACE)"
	./hack/verify-workspace-isolation.sh "$(WORKSPACE_NAMESPACE)" "$(LAKE_NAMESPACE)" "$(LAKE_NAME)" "$(OTHER_WORKSPACE_NAMESPACE)"
