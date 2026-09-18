SHELL := bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help
VERSION := $(shell node -p "require('./package.json').version")
LDFLAGS := -X github.com/tyk-swe/olp/internal/process.Version=$(VERSION)

.PHONY: help setup dev check test integration api build fmt

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

check: api ## Check formatting, vet, Go behavior, and the console
	@test -z "$$(gofmt -l cmd internal openapi/*.go tests/fixtures/*.go tests/integration tests/sdkfixture)" || { gofmt -l cmd internal openapi/*.go tests/fixtures/*.go tests/integration tests/sdkfixture; exit 1; }
	go vet ./...
	$(MAKE) test
	node --test scripts/release-inventory.test.mjs
	pnpm --dir console verify

test: ## Run Go unit and protocol tests without containers
	go test ./...

integration: ## Run isolated service, race, process, SDK, and browser checks
	./scripts/integration.sh

build: api ## Build the native Go binary and static console
	mkdir -p .local/bin
	CGO_ENABLED=1 go build -mod=readonly -trimpath -ldflags='$(LDFLAGS)' -o .local/bin/olp ./cmd/olp
	pnpm --dir console build

fmt: ## Format Go and console source
	gofmt -w cmd internal openapi/*.go tests/fixtures/*.go tests/integration tests/sdkfixture
	pnpm --dir console format
