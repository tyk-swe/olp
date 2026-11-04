SHELL := bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help
VERSION = $(shell node -p "require('./package.json').version")
LDFLAGS = -X main.Version=$(VERSION)

GO_BUILD_OUTPUT ?= .local/bin/olp
GO_BUILD_TAGS ?=
SIGNED_DOCUMENTS = internal/catalog/catalog.json internal/pluginindex/index.json
GOFMT_PATHS = \
	cmd guardrails inference internal management plugin \
	tests/fidelity tests/fixtures/*.go tests/fixtures/fidelity

.PHONY: \
	help setup dev check check-fast lint fmt-check vet \
	test-fast \
	api build build-go catalog catalog-sign fmt

help: ## Show development commands
	@awk 'BEGIN {FS = ":.*## "} /^[a-z-]+:.*## / {printf "  %-16s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

setup: ## Install Go and console dependencies and generate contracts
	go mod download
	pnpm install --frozen-lockfile
	$(MAKE) api

dev: ## Start isolated services, Go, and Vite
	./scripts/dev.sh

check: api ## Check formatting, vet, lint, console types/lint, and the fast safety net
	$(MAKE) fmt-check vet lint
	pnpm --dir console format:check
	pnpm --dir console check
	$(MAKE) test-fast

check-fast: ## Check formatting, vet, lint, and the fast safety net without regenerating contracts
	$(MAKE) fmt-check vet lint
	$(MAKE) test-fast

fmt-check: ## Fail when Go source is not gofmt formatted
	@bad="$$(gofmt -l $(GOFMT_PATHS))"; test -z "$$bad" || { printf '%s\n' "$$bad"; exit 1; }

vet: ## Run go vet on all packages
	go vet ./...

lint: ## Run the pinned Go static analysis gate (see staticcheck.conf)
	./scripts/check-lint.sh

test-fast: ## Run the fast safety net inside its 60 second budget
	./scripts/test-fast.sh

api: ## Generate Go and TypeScript contracts without compiling the gateway
	./scripts/api.sh

build: api ## Build the native Go binary and static console
	$(MAKE) build-go
	pnpm --dir console build

build-go: ## Build the native Go binary from generated contracts
	mkdir -p -- "$$(dirname -- "$(GO_BUILD_OUTPUT)")"
	CGO_ENABLED=1 go build -mod=readonly -trimpath $(if $(strip $(GO_BUILD_TAGS)),-tags="$(GO_BUILD_TAGS)") -ldflags='$(LDFLAGS)' -o "$(GO_BUILD_OUTPUT)" ./cmd/olp

catalog: ## Rewrite the reference catalog and plugin index in canonical form
	go run ./internal/signing/cmd/olpsign fmt $(SIGNED_DOCUMENTS)

catalog-sign: catalog ## Sign the reference catalog and plugin index with the development key
	go run ./internal/signing/cmd/olpsign sign -key-id dev-2026a -seed-file internal/signing/devkey/dev-2026a.seed $(SIGNED_DOCUMENTS)

fmt: ## Format Go and console source
	gofmt -w $(GOFMT_PATHS)
	pnpm --dir console format
