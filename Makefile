SHELL := bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help
VERSION = $(shell node -p "require('./package.json').version")
LDFLAGS = -X github.com/tyk-swe/olp/internal/process.Version=$(VERSION)

GO_TEST_PACKAGES ?= ./...
GO_TEST_ARGS ?=
GO_TEST_TIMEOUT ?= 5m
CONSOLE_TEST_ARGS ?=

.PHONY: help setup dev check test test-go test-console test-scripts test-race integration api build fmt

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
	@test -z "$$(gofmt -l cmd internal openapi/*.go tests/fixtures/*.go tests/integration tests/sdkfixture)" || { gofmt -l cmd internal openapi/*.go tests/fixtures/*.go tests/integration tests/sdkfixture; exit 1; }
	go vet ./...
	pnpm --dir console format:check
	pnpm --dir console check
	$(MAKE) test

test: test-go test-console test-scripts ## Run Go, console, and script tests without containers

test-go: ## Run Go unit and protocol tests
	go test -mod=readonly -timeout=$(GO_TEST_TIMEOUT) $(GO_TEST_ARGS) $(GO_TEST_PACKAGES)

test-console: ## Run console unit and component tests
	pnpm --dir console test $(CONSOLE_TEST_ARGS)

test-scripts: ## Run automation script tests
	node --test scripts/*.test.mjs

test-race: ## Run uncached Go tests with race detection
	$(MAKE) test-go GO_TEST_ARGS="$(GO_TEST_ARGS) -race -count=1"

integration: ## Run isolated service, race, process, SDK, and browser checks
	./scripts/integration.sh

build: api ## Build the native Go binary and static console
	mkdir -p .local/bin
	CGO_ENABLED=1 go build -mod=readonly -trimpath -ldflags='$(LDFLAGS)' -o .local/bin/olp ./cmd/olp
	pnpm --dir console build

fmt: ## Format Go and console source
	gofmt -w cmd internal openapi/*.go tests/fixtures/*.go tests/integration tests/sdkfixture
	pnpm --dir console format
