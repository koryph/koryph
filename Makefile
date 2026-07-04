# SPDX-License-Identifier: Apache-2.0
# Copyright (c) 2026 The Koryph Developers
#
# Self-documenting Makefile: `make` or `make help` lists all targets.
# Groups are declared with ##@ markers; target docs with trailing ## comments.

.DEFAULT_GOAL := help
SHELL := /bin/sh
BIN   := bin/koryph

##@ General

.PHONY: help
help: ## Display this help, grouped by section
	@awk 'BEGIN {FS = ":.*##"; printf "\nkoryph — make targets\n\nUsage:\n  make \033[36m<target>\033[0m\n"} \
	/^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } \
	/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Development

.PHONY: init
init: ## Bootstrap a fresh clone: pinned tools (nix), git hooks, and koryph
	@command -v nix >/dev/null 2>&1 || { \
	  echo "Nix is required for the pinned dev toolchain but was not found."; \
	  echo "Install Determinate Nix (recommended):"; \
	  echo "  curl --proto '=https' --tlsv1.2 -sSf -L https://install.determinate.systems/nix | sh -s -- install"; \
	  echo "  docs: https://docs.determinate.systems/"; \
	  exit 1; }
	@command -v direnv >/dev/null 2>&1 && { direnv allow . 2>/dev/null || true; } || \
	  echo "note: direnv not found (optional) — enter the shell with 'nix develop': https://direnv.net"
	nix develop --command pre-commit install-hooks
	nix develop --command go install ./cmd/koryph
	@echo ""
	@echo "koryph dev environment ready:"
	@echo "  - tools pinned via flake.nix ('nix develop', or automatically via direnv)"
	@echo "  - git hooks: owned by beads (core.hooksPath=.beads/hooks); they chain to pre-commit"
	@echo "  - koryph installed to \$$(go env GOPATH)/bin (ensure it is on PATH)"
	@echo "  next — register this project so koryph can build itself:"
	@echo "    koryph project add . --account personal --identity you@example.com"
	@echo "    koryph signing enable --project koryph   # if signing is required"

.PHONY: build
build: ## Build the koryph binary into bin/
	go build -o $(BIN) ./cmd/koryph

.PHONY: install
install: ## Install koryph into GOPATH/bin (~/bin)
	go install ./cmd/koryph

.PHONY: run
run: build ## Build then show the CLI help
	$(BIN)

.PHONY: clean
clean: ## Remove build and docs artifacts
	rm -rf bin dist site

##@ Quality

.PHONY: fmt
fmt: ## gofmt all Go sources in place
	gofmt -w .

.PHONY: fmt-check
fmt-check: ## Fail if any Go source is not gofmt-clean
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "gofmt: files above need formatting"; exit 1; }

.PHONY: vet
vet: ## go vet all packages
	go vet ./...

.PHONY: test
test: ## Run the full test suite
	go test ./...

.PHONY: test-race
test-race: ## Run the full test suite with the race detector
	go test -race ./...

.PHONY: cover
cover: ## Run tests with coverage summary
	go test -cover ./...

# Per-checkout lint cache: the default shared cache serves stale results
# across git worktrees (keyed by relative paths that collide), which has
# produced phantom gate failures citing files in deleted worktrees.
export GOLANGCI_LINT_CACHE := $(CURDIR)/.cache/golangci-lint

.PHONY: lint
lint: ## Run golangci-lint (skipped with a notice if not installed; CI enforces it)
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; \
	else echo "golangci-lint not installed; skipping (CI enforces it) — see .golangci.yml"; fi

.PHONY: lint-ci
lint-ci: ## Enforced lint for CI (pins golangci-lint via go run, like make vuln)
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run ./...

.PHONY: reuse
reuse: ## REUSE/SPDX compliance (skipped with a notice if no runner; CI enforces it)
	@if command -v reuse >/dev/null 2>&1; then reuse lint; \
	elif command -v uvx >/dev/null 2>&1; then uvx reuse lint; \
	elif command -v pipx >/dev/null 2>&1; then pipx run reuse lint; \
	else echo "reuse not installed (try 'uvx reuse lint'); skipping — CI enforces it"; fi

.PHONY: gate
gate: fmt-check build vet test lint reuse ## The green gate (mirrors koryph.project.json)

##@ VS Code Extension

.PHONY: ext-build
ext-build: ## Build the VS Code extension bundle in ide/vscode/ (skipped with a notice if npm is absent)
	@command -v npm >/dev/null 2>&1 || { echo "npm not installed; skipping ext-build (CI enforces it) — see ide/vscode/"; exit 0; }
	cd ide/vscode && npm ci --no-fund --no-audit && npm run bundle

.PHONY: ext-test
ext-test: ## Run the VS Code extension unit test suite in ide/vscode/ (skipped with a notice if npm is absent)
	@command -v npm >/dev/null 2>&1 || { echo "npm not installed; skipping ext-test (CI enforces it) — see ide/vscode/"; exit 0; }
	cd ide/vscode && npm ci --no-fund --no-audit && npm test

##@ Documentation

.PHONY: docs-serve
docs-serve: ## Serve the mkdocs book locally (requires mkdocs-material)
	mkdocs serve

.PHONY: docs-build
docs-build: ## Build the mkdocs book strictly into site/
	mkdocs build --strict

##@ Release

.PHONY: version
version: ## Print the engine version
	@go run ./cmd/koryph version

.PHONY: version-check
version-check: ## Verify internal/version.Engine matches TAG (make version-check TAG=v0.3.0)
	@test -n "$(TAG)" || { echo "usage: make version-check TAG=vX.Y.Z"; exit 2; }
	@have="v$$(go run ./cmd/koryph version | awk '{print $$NF}')"; \
	  { test "$$have" = "$(TAG)" && echo "version aligned: $$have"; } || \
	  { echo "version mismatch: engine $$have != tag $(TAG)"; exit 1; }

.PHONY: release-snapshot
release-snapshot: ## Build a local release snapshot (goreleaser, no publish)
	@command -v goreleaser >/dev/null 2>&1 || { echo "goreleaser not found — provided by the nix dev shell (nix develop)"; exit 1; }
	goreleaser release --snapshot --clean

.PHONY: vuln
vuln: ## Scan for known Go vulnerabilities (govulncheck)
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

.PHONY: sbom
sbom: ## Generate an SPDX SBOM of the module into dist/ (requires syft)
	@command -v syft >/dev/null 2>&1 || { echo "syft not found — install from https://github.com/anchore/syft"; exit 1; }
	@mkdir -p dist
	syft scan dir:. --source-name koryph -o spdx-json=dist/koryph.spdx.json
	@echo "wrote dist/koryph.spdx.json"

##@ Repo administration (IaC — requires admin-scoped gh auth)

.PHONY: repo-check
repo-check: ## Diff live GitHub settings/rulesets against .github IaC (exit 1 on drift)
	@fail=0; for s in scripts/ensure-*.sh; do "$$s" --check || fail=1; done; exit $$fail

.PHONY: repo-apply
repo-apply: ## Apply .github IaC (rulesets, repo settings) to the live repo
	@for s in scripts/ensure-*.sh; do "$$s" --apply; done
