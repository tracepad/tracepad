# Tracepad development targets. `make help` lists them.

.DEFAULT_GOAL := help

BINARY  := tracepad
VERSION ?= dev

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-18s %s\n", $$1, $$2}'

build: ## Build the binary into ./bin
	go build -ldflags "-s -w -X main.version=$(VERSION)" -o bin/$(BINARY) ./cmd/tracepad

dev: ## Run the server locally, mirroring output to .dev.log
	go run ./cmd/tracepad serve 2>&1 | tee .dev.log

test: ## Run all tests
	go test ./...

vet: ## Static checks
	go vet ./...

smoke: ## Export from real SDKs into a real binary and assert the rows
	scripts/smoke/run.sh

fixtures: ## Regenerate testdata/otlp bodies and their golden files
	go test ./internal/mapping -run TestGoldenFixtures -update

format: ## Format all Go sources
	gofmt -w .

format-check: ## Fail if any file is unformatted (read-only, used by CI/gate)
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "unformatted files:"; echo "$$out"; exit 1; fi

precommit: ensure-hooks format-check vet test ## Full gate (also installed as git pre-commit hook)

# git rev-parse --git-path resolves the hooks dir in worktrees too; empty
# outside a git checkout (e.g. a source tarball), where hooks don't apply.
HOOKS_DIR = $(shell git rev-parse --git-path hooks 2>/dev/null)

ensure-hooks: ## Install the pre-commit gate hook if missing (no-op otherwise)
	@if [ -n "$(HOOKS_DIR)" ] && [ ! -e "$(HOOKS_DIR)/pre-commit" ]; then \
		printf '#!/bin/sh\nexec make precommit\n' > "$(HOOKS_DIR)/pre-commit"; \
		chmod +x "$(HOOKS_DIR)/pre-commit"; \
		echo "installed pre-commit gate hook"; \
	fi

install-hooks: ## (Re)install the pre-commit gate hook
	printf '#!/bin/sh\nexec make precommit\n' > "$(HOOKS_DIR)/pre-commit"
	chmod +x "$(HOOKS_DIR)/pre-commit"

.PHONY: help build dev test vet smoke fixtures format format-check precommit ensure-hooks install-hooks
