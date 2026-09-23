# Tracepad development targets. `make help` lists them.

.DEFAULT_GOAL := help

BINARY  := tracepad
VERSION ?= dev

# The web interface (spec 006). It is built by Node and staged into the
# package the `ui` build tag embeds; neither directory is committed.
UI       := ui
UI_DIST  := $(UI)/dist
UI_EMBED := internal/ui/dist
UI_TYPES := $(UI)/src/lib/api/schema.d.ts

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-18s %s\n", $$1, $$2}'

build: ui ## Build the binary, web interface included, into ./bin
	go build -tags ui -ldflags "-s -w -X main.version=$(VERSION)" -o bin/$(BINARY) ./cmd/tracepad

build-server: ## Build without the web interface (no Node required; serves the stub page)
	go build -ldflags "-s -w -X main.version=$(VERSION)" -o bin/$(BINARY) ./cmd/tracepad

dev: ## Run the server locally, mirroring output to .dev.log
	go run ./cmd/tracepad serve 2>&1 | tee .dev.log

test: ## Run all tests
	go test ./...

vet: ## Static checks
	go vet ./...

smoke: ## Export from real SDKs into a real binary and assert the rows
	scripts/smoke/run.sh

# Three bodies in the corpus are not synthetic: `010-tracepad-sdk.pb` is what
# the Python package puts on the wire (spec 017, Testing), `013-tracepad-sdk-js.pb`
# what the Node package does (spec 032 #12) and `014-tracepad-sdk-go.pb` what
# the Go package does (spec 033 #12). They are rewritten first, by each
# package's own exporter — the Python one under the pinned SDKs of the smoke
# test — and the Go pass then regenerates every golden, including theirs, from
# the bytes on disk. Without `uv` or `npm` the committed body stands and only
# the goldens move, which is what a Go-only checkout wants anyway.
fixtures: ## Regenerate testdata/otlp bodies and their golden files
	@if command -v uv >/dev/null 2>&1; then \
		uv run --quiet --isolated --with-requirements scripts/smoke/requirements.txt \
			--with-editable sdk/python python scripts/fixtures/tracepad_sdk.py; \
	else \
		echo "uv is missing: keeping testdata/otlp/010-tracepad-sdk.pb as committed"; \
	fi
	@if command -v npm >/dev/null 2>&1; then \
		$(MAKE) sdk-js-build && node scripts/fixtures/tracepad_sdk_js.mjs; \
	else \
		echo "npm is missing: keeping testdata/otlp/013-tracepad-sdk-js.pb as committed"; \
	fi
	cd sdk/go && go run ./internal/fixture ../../testdata/otlp/014-tracepad-sdk-go.pb
	go test ./internal/mapping -run TestGoldenFixtures -update

format: ## Format all Go sources
	gofmt -w .

format-check: ## Fail if any file is unformatted (read-only, used by CI/gate)
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "unformatted files:"; echo "$$out"; exit 1; fi

# --- Web interface (spec 006) -------------------------------------------------
#
# Node and npm are dev prerequisites of the UI half only (Decision 10): every
# Go target above runs without them, and so does the whole Go test suite.

ui-deps: ## Install the UI toolchain if node_modules is missing
	@if [ ! -d $(UI)/node_modules ]; then \
		command -v npm >/dev/null || { echo "npm is required to build the web interface (see ui/package.json engines)"; exit 1; }; \
		cd $(UI) && npm ci; \
	fi

ui: ui-deps ## Build the SPA and stage it where the `ui` build tag embeds it
	cd $(UI) && npm run build
	rm -rf $(UI_EMBED)
	mkdir -p $(UI_EMBED)
	cp -R $(UI_DIST)/. $(UI_EMBED)/

ui-types: ui-deps ## Regenerate the TypeScript API types from openapi.json
	cd $(UI) && npm run types

# The drift check regenerates in place and asks git whether anything moved
# (Decision 7): a failure leaves the corrected file in the tree, so the fix is
# to review it and commit it. Outside a checkout there is nothing to compare
# against, and the check is skipped rather than failed.
ui-types-check: ui-types ## Fail if the committed API types no longer match openapi.json
	@git rev-parse --git-dir >/dev/null 2>&1 || { echo "not a git checkout, skipping the type drift check"; exit 0; }; \
	git diff --exit-code -- $(UI_TYPES) >/dev/null || { \
		echo "$(UI_TYPES) is out of date with internal/server/openapi.json;"; \
		echo "it has just been regenerated — review the diff and commit it."; \
		exit 1; \
	}

ui-check: ui-deps ui-types-check ## Type-check the SPA and run its unit tests
	cd $(UI) && npm run check
	cd $(UI) && npm run test

# --- The image (spec 020) -----------------------------------------------------
#
# Docker is a prerequisite of these two targets only, the way Node is of the
# interface and `uv` is of the package: nothing above needs a daemon.

IMAGE     := tracepad
IMAGE_TAG := dev

image: ## Build the Docker image as tracepad:dev
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(IMAGE_TAG) .

# The version the check expects is the one `image` stamped, so the two targets
# cannot disagree about what a passing run proves.
image-check: ## Boot the image on an ephemeral volume and assert the contract
	EXPECT_VERSION=$(VERSION) scripts/image-check.sh $(IMAGE):$(IMAGE_TAG)

e2e: build ## Boot the real binary on a temp database and run the Playwright smoke
	cd $(UI) && npx playwright install chromium
	cd $(UI) && npm run e2e

# The budget is named here as well as defaulted in the script (spec 015 #9), so
# that the number a build reports is visible in the target that reports it.
UI_BUDGET := 21800

ui-lines: ## Report the interface's application lines against its budget, and its test lines beside them
	scripts/ui-lines.sh $(UI_BUDGET)

# --- The Python package (specs 017, 018) --------------------------------------
#
# `uv` is a dev prerequisite of the package half only, the way Node is of the
# interface: every target above runs without it.

sdk-test: ## Unit-test the Python package, and end-to-end against a real binary
	scripts/sdk-test.sh

# The budget spec 017 #1 set, shared with the harness of spec 018; raised for
# the streaming pass-through (spec 031 #22), for trace deletion (spec 036 #8),
# for `tracepad.testing` (spec 040 #13) and for the cost and bounds of spec 042.
SDK_BUDGET := 2000

sdk-lines: ## Report the Python package's application lines against its budget
	scripts/sdk-lines.sh $(SDK_BUDGET)

# --- The Go package (spec 033) ------------------------------------------------
#
# A nested module (`sdk/go/go.mod`), so that a user's `go get` does not carry
# the server's dependency graph — which is also why the root `go test ./...`
# does not see it: it is named here, and in the gate, explicitly.

sdk-go-test: ## Vet and unit-test the Go package, and end-to-end against a real binary
	scripts/sdk-go-test.sh

sdk-go-unit: ## The Go package's vet and unit tests alone (part of the gate)
	SDK_SKIP_E2E=1 scripts/sdk-go-test.sh

# The budget spec 033 #11 set, raised for the harness (spec 033 #17), for
# trace deletion (spec 036 #8), for `tracepadtest` (spec 040 #13) and for the
# cost and bounds of spec 042.
SDK_GO_BUDGET := 2500

sdk-go-lines: ## Report the Go package's application lines against its budget
	scripts/sdk-go-lines.sh $(SDK_GO_BUDGET)

# --- The Node package (spec 032) ----------------------------------------------
#
# Node and npm are dev prerequisites of this half the way they are of the
# interface: every Go target runs without them.

SDK_JS := sdk/js

sdk-js-deps: ## Install the Node package's toolchain if node_modules is missing
	@if [ ! -d $(SDK_JS)/node_modules ]; then \
		command -v npm >/dev/null || { echo "npm is required for the Node package (see sdk/js/package.json engines)"; exit 1; }; \
		cd $(SDK_JS) && npm ci; \
	fi

sdk-js-build: sdk-js-deps ## Build the Node package into sdk/js/dist
	cd $(SDK_JS) && npm run build

sdk-js-test: ## Type-check and unit-test the Node package, and end-to-end against a real binary
	scripts/sdk-js-test.sh

# The budget spec 032 #11 set, shared with the harness of the same spec;
# raised for the harness (spec 032 #16), for trace deletion (spec 036 #8), for
# `tracepad/testing` (spec 040 #13) and for the cost and bounds of spec 042.
SDK_JS_BUDGET := 2275

sdk-js-lines: ## Report the Node package's application lines against its budget
	scripts/sdk-js-lines.sh $(SDK_JS_BUDGET)

# The documentation's own cross-references (spec 026 #6): a hundred anchors
# nothing read until now. Cheap enough for the gate — it is awk over seventeen
# files — and the failure it catches is invisible in review.
doc-anchors: ## Check every anchor in docs/, README.md and AGENTS.md against its heading
	scripts/doc-anchors.sh

doc-anchors-self-test: ## Assert the anchor checker against its fixture
	scripts/doc-anchors.sh --self-test

gate: ensure-hooks format-check vet test sdk-go-unit doc-anchors ui-check ## Full gate: what CI runs, and the git pre-push hook

# The pre-commit hook runs this: the checks that are cheap and the tests of
# what is actually staged. The full gate runs once per push instead of once
# per commit — a branch of twenty commits was paying two minutes each for
# `go test ./...` and the whole interface suite, and CI runs both anyway.
precommit: ensure-hooks format-check vet test-staged ui-check-staged ## Fast gate for the pre-commit hook: staged Go packages, the interface only when it changed

# Only the packages with a staged .go file. A change that breaks a dependent
# package is caught by the pre-push gate, not here. The Go package is its own
# module: a staged file under sdk/go runs that module's unit suite instead.
test-staged: ## Go tests of the packages with staged changes
	@staged=$$(git diff --cached --name-only --diff-filter=ACMR -- '*.go'); \
	pkgs=$$(printf '%s\n' "$$staged" | grep -v '^sdk/go/' | xargs -n1 dirname 2>/dev/null | sort -u | sed 's|^|./|'); \
	if [ -n "$$pkgs" ]; then go test $$pkgs || exit 1; else echo "test-staged: no Go changes staged"; fi; \
	if printf '%s\n' "$$staged" | grep -q '^sdk/go/'; then $(MAKE) sdk-go-unit; fi

ui-check-staged: ## The interface's type-check and unit tests, only when ui/ or openapi.json is staged
	@if git diff --cached --name-only --diff-filter=ACMR | grep -qE '^(ui/|internal/server/openapi\.json$$)'; then \
		$(MAKE) ui-check; \
	else echo "ui-check-staged: no interface changes staged"; fi

# git rev-parse --git-path resolves the hooks dir in worktrees too (worktrees
# share it); empty outside a git checkout (e.g. a source tarball), where hooks
# don't apply. The hooks run with the working tree as the current directory.
HOOKS_DIR = $(shell git rev-parse --git-path hooks 2>/dev/null)

ensure-hooks: ## Install the pre-commit and pre-push hooks if missing (no-op otherwise)
	@if [ -n "$(HOOKS_DIR)" ] && [ ! -e "$(HOOKS_DIR)/pre-commit" ]; then \
		printf '#!/bin/sh\nexec make precommit\n' > "$(HOOKS_DIR)/pre-commit"; \
		chmod +x "$(HOOKS_DIR)/pre-commit"; \
		echo "installed pre-commit hook"; \
	fi
	@if [ -n "$(HOOKS_DIR)" ] && [ ! -e "$(HOOKS_DIR)/pre-push" ]; then \
		printf '#!/bin/sh\nexec make gate\n' > "$(HOOKS_DIR)/pre-push"; \
		chmod +x "$(HOOKS_DIR)/pre-push"; \
		echo "installed pre-push gate hook"; \
	fi

install-hooks: ## (Re)install both hooks
	printf '#!/bin/sh\nexec make precommit\n' > "$(HOOKS_DIR)/pre-commit"
	chmod +x "$(HOOKS_DIR)/pre-commit"
	printf '#!/bin/sh\nexec make gate\n' > "$(HOOKS_DIR)/pre-push"
	chmod +x "$(HOOKS_DIR)/pre-push"

.PHONY: help build build-server dev test vet smoke fixtures format format-check \
	ui ui-deps ui-types ui-types-check ui-check ui-lines image image-check \
	e2e sdk-test sdk-lines sdk-go-test sdk-go-unit sdk-go-lines \
	sdk-js-deps sdk-js-build sdk-js-test sdk-js-lines \
	doc-anchors doc-anchors-self-test gate precommit \
	test-staged ui-check-staged ensure-hooks install-hooks
