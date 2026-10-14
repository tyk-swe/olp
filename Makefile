SHELL := bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help
VERSION = $(shell node -p "require('./package.json').version")
LDFLAGS = -X github.com/tyk-swe/olp/internal/process.Version=$(VERSION)

GO_TEST_PACKAGES ?= ./...
GO_TEST_ARGS ?=
GO_TEST_TIMEOUT ?= 5m
CONSOLE_TEST_ARGS ?=
GO_BUILD_OUTPUT ?= .local/bin/olp
GO_BUILD_TAGS ?=
SIGNED_DOCUMENTS = internal/catalog/catalog.json internal/pluginindex/index.json
GOFMT_PATHS = \
	cmd internal openapi plugins sdk \
	tests/bench tests/clientfixture tests/clients/gosdk tests/codecli \
	tests/fidelity tests/fixtures/*.go tests/fixtures/codex-qualified \
	tests/fixtures/fidelity tests/integration tests/samlfixture tests/sdkfixture

.PHONY: \
	help setup dev check \
	test test-go test-bench test-console test-scripts test-race integration \
	bench bench-compare bench-gate \
	api build build-go catalog catalog-sign fmt

help: ## Show development commands
	@awk 'BEGIN {FS = ":.*## "} /^[a-z-]+:.*## / {printf "  %-16s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

setup: ## Install Go and console dependencies and generate contracts
	go mod download
	pnpm install --frozen-lockfile
	$(MAKE) api

dev: ## Start isolated services, Go, and Vite
	./scripts/dev.sh

check: api ## Check formatting, vet, console types/lint, and all local tests
	@bad="$$(gofmt -l $(GOFMT_PATHS))"; test -z "$$bad" || { printf '%s\n' "$$bad"; exit 1; }
	go vet ./...
	go vet -tags=bench ./tests/bench/...
	pnpm --dir console format:check
	pnpm --dir console check
	$(MAKE) test

test: test-go test-bench test-console test-scripts ## Run Go, benchmark harness, console, and script tests without containers

test-go: ## Run Go unit and protocol tests
	go test -mod=readonly -timeout=$(GO_TEST_TIMEOUT) $(GO_TEST_ARGS) $(GO_TEST_PACKAGES)

test-bench: ## Run the benchmark harness's own tests, which need no services
	go test -mod=readonly -tags=bench -timeout=$(GO_TEST_TIMEOUT) $(GO_TEST_ARGS) -skip '^TestScenario' ./tests/bench/...

test-console: ## Run console unit and component tests
	pnpm --dir console test $(CONSOLE_TEST_ARGS)

test-scripts: ## Run automation script tests
	node --test scripts/*.test.mjs

test-race: ## Run uncached Go tests with race detection
	$(MAKE) test-go GO_TEST_ARGS="$(GO_TEST_ARGS) -race -count=1"
	$(MAKE) test-bench GO_TEST_ARGS="$(GO_TEST_ARGS) -race -count=1"

integration: ## Run isolated service, race, process, SDK, and browser checks
	./scripts/integration.sh

bench: ## Run the gateway benchmark scenarios against the mock upstream
	./scripts/bench.sh

bench-compare: ## Run the benchmark scenarios against OLP and the pinned LiteLLM release
	./scripts/bench-compare.sh

bench-gate: ## Compare hot-path benchmarks with the merge base
	./scripts/bench-gate.sh

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
