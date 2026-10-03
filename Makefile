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

.PHONY: help setup dev check test test-go test-bench test-console test-scripts test-race integration bench bench-compare bench-gate api build build-go fmt

help: ## Show development commands
	@awk 'BEGIN {FS = ":.*## "} /^[a-z-]+:.*## / {printf "  %-16s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

setup: ## Install Go and console dependencies and generate contracts
	go mod download
	pnpm install --frozen-lockfile
	$(MAKE) api

dev: ## Start isolated services, Go, and Vite
	./scripts/dev.sh

api: ## Generate Go and TypeScript contracts without compiling the gateway
	./scripts/api.sh

check: api ## Check formatting, vet, console types/lint, and all local tests
	@test -z "$$(gofmt -l cmd internal sdk openapi/*.go tests/fixtures/*.go tests/fixtures/fidelity tests/fidelity tests/integration tests/sdkfixture $(wildcard tests/bench tests/clients))" || { gofmt -l cmd internal sdk openapi/*.go tests/fixtures/*.go tests/fixtures/fidelity tests/fidelity tests/integration tests/sdkfixture $(wildcard tests/bench tests/clients); exit 1; }
	go vet ./...
	@if [ -n "$$(find tests/bench -name '*.go' -print -quit 2>/dev/null)" ]; then go vet -tags=bench ./tests/bench/...; fi
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

build: api ## Build the native Go binary and static console
	$(MAKE) build-go
	pnpm --dir console build

build-go: ## Build the native Go binary from generated contracts
	mkdir -p -- "$$(dirname -- "$(GO_BUILD_OUTPUT)")"
	CGO_ENABLED=1 go build -mod=readonly -trimpath $(if $(strip $(GO_BUILD_TAGS)),-tags="$(GO_BUILD_TAGS)") -ldflags='$(LDFLAGS)' -o "$(GO_BUILD_OUTPUT)" ./cmd/olp

fmt: ## Format Go and console source
	gofmt -w cmd internal sdk openapi/*.go tests/fixtures/*.go tests/fixtures/fidelity tests/fidelity tests/integration tests/sdkfixture $(wildcard tests/bench tests/clients)
	pnpm --dir console format
