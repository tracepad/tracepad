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

format: ## Format all Go sources
	gofmt -w .

format-check: ## Fail if any file is unformatted (read-only, used by CI/gate)
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "unformatted files:"; echo "$$out"; exit 1; fi

precommit: format-check vet test ## Full gate (also installed as git pre-commit hook)

install-hooks: ## Install the pre-commit gate hook
	printf '#!/bin/sh\nexec make precommit\n' > .git/hooks/pre-commit
	chmod +x .git/hooks/pre-commit

.PHONY: help build dev test vet format format-check precommit install-hooks
