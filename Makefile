VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all
all: build

##@ Development

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: web
web: ## Build the page with Vite and copy it into the Go embed directory.
	cd web && npm run build
	rm -rf internal/web/dist && cp -R web/dist internal/web/dist && touch internal/web/dist/.gitkeep

.PHONY: build
build: web fmt vet ## Build the llamesh binary with the page embedded.
	go build -ldflags "-X main.version=$(VERSION)" -o llamesh ./cmd/llamesh

.PHONY: test
# JUNIT_REPORT=<file> additionally writes a JUnit XML report (via gotestsum).
test: fmt vet $(if $(JUNIT_REPORT),gotestsum) ## Run unit tests.
	$(if $(JUNIT_REPORT),$(GOTESTSUM) --junitfile $(abspath $(JUNIT_REPORT)) --format standard-verbose --,go test) \
		-race ./... -coverprofile cover.out
	go tool cover -func=cover.out | tail -1

# LLAMA and MODEL name a llama.cpp release directory and a GGUF; see test/linux-e2e.sh.
.PHONY: test-e2e
test-e2e: ## Run the Linux end to end: a CPU llama-server under systemd, split to an RPC node.
	LLAMA=$(LLAMA) MODEL=$(MODEL) $(if $(OUT),OUT=$(OUT)) test/linux-e2e.sh

.PHONY: lint
lint: golangci-lint ## Run golangci-lint.
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint with --fix.
	$(GOLANGCI_LINT) run --fix

# The contract in proto/ generates the Go types and the page's types; CI fails when they are stale.
.PHONY: proto
proto: ## Lint the contract and regenerate its code.
	buf lint && buf generate

##@ Dependencies

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
GOLANGCI_LINT_VERSION ?= v2.12.2

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT)
$(GOLANGCI_LINT): $(LOCALBIN)
	test -s $(GOLANGCI_LINT) && $(GOLANGCI_LINT) version | grep -q $(GOLANGCI_LINT_VERSION:v%=%) || \
	GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

GOTESTSUM = $(LOCALBIN)/gotestsum
GOTESTSUM_VERSION ?= v1.13.0

.PHONY: gotestsum
gotestsum: $(GOTESTSUM)
$(GOTESTSUM): $(LOCALBIN)
	test -s $(GOTESTSUM) || GOBIN=$(LOCALBIN) go install gotest.tools/gotestsum@$(GOTESTSUM_VERSION)

##@ Help

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
