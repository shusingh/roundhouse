# Roundhouse developer entry point. Run `make help` for the target list.
#
# Works with GNU Make on Linux and on Windows (Git Bash). Firecracker needs
# Linux and KVM, so on Windows the Linux-only targets run inside WSL2 on this
# same checkout (see docs/adr/0008-wsl2-development-host.md).

SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

GO            ?= go
GOLANGCI_LINT ?= golangci-lint
BIN_DIR       := bin
HOST_BINARIES := roundhoused rhctl rhbench
GUEST_BINARY  := roundhouse-agent

ifeq ($(OS),Windows_NT)
  # 64-bit shells find wsl.exe on PATH; 32-bit shells only see it through
  # Sysnative, because System32 is redirected for them.
  WSL   := $(shell command -v wsl.exe 2>/dev/null || echo /c/Windows/Sysnative/wsl.exe)
  LINUX := MSYS_NO_PATHCONV=1 $(WSL) -d Ubuntu-24.04 --cd "$(CURDIR)" -- bash -lc
else
  LINUX := bash -lc
endif

.PHONY: help
help: ## Show this help.
	@grep -hE '^[a-zA-Z0-9_-]+:.*## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

# Build

.PHONY: build
build: ## Build Linux binaries (host tools and the static guest agent) into ./bin.
	@mkdir -p $(BIN_DIR)
	@for b in $(HOST_BINARIES) $(GUEST_BINARY); do \
		if [ -d cmd/$$b ]; then \
			CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -o $(BIN_DIR)/$$b ./cmd/$$b; \
		fi; \
	done

# Test

.PHONY: test
test: ## Run portable unit tests with the race detector on this machine.
	CGO_ENABLED=1 $(GO) test -race -count=1 ./...

.PHONY: test-linux
test-linux: ## Run all unit tests, including Linux-only code, inside Linux.
	$(LINUX) 'CGO_ENABLED=1 go test -race -count=1 ./...'

.PHONY: integration
integration: build ## Run tests that boot real microVMs (needs KVM and root).
	$(LINUX) 'sudo -E env "PATH=$$PATH" go test -count=1 -tags integration ./test/integration/...'

.PHONY: containment
containment: build ## Run the hostile-workload containment suite (needs KVM and root).
	$(LINUX) 'sudo -E env "PATH=$$PATH" go test -count=1 -tags containment -timeout 30m ./test/containment/...'

.PHONY: cover
cover: ## Run tests with coverage and print the total.
	CGO_ENABLED=1 $(GO) test -race -count=1 -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -n 1

# Code quality

.PHONY: lint
lint: ## Run golangci-lint for this platform and for Linux.
	$(GOLANGCI_LINT) run ./...
	GOOS=linux $(GOLANGCI_LINT) run ./...

.PHONY: fmt
fmt: ## Format code with gofmt and goimports rules.
	$(GOLANGCI_LINT) fmt ./...

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum.
	$(GO) mod tidy

ifeq ($(OS),Windows_NT)
.PHONY: check
check: tidy lint test test-linux build ## Everything CI runs, on Windows and in WSL.
else
.PHONY: check
check: tidy lint test build ## Everything CI runs.
endif

# Environment

.PHONY: setup
setup: ## Install git hooks and verify the toolchain.
	./scripts/setup-dev.sh

.PHONY: host-setup
host-setup: ## Prepare the Linux host: packages, KVM access, Firecracker (runs as root).
	$(LINUX) 'sudo bash deploy/host-setup.sh'

.PHONY: clean
clean: ## Remove build and coverage output.
	rm -rf $(BIN_DIR) coverage.out
