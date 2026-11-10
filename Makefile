SHELL := bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help
VERSION = $(shell node -p "require('./package.json').version")
LDFLAGS = -X main.Version=$(VERSION)

GO_BUILD_OUTPUT ?= .local/bin/olp
GO_BUILD_TAGS ?=
AIR_VERSION := v1.67.4
SIGNED_DOCUMENTS = internal/catalog/catalog.json internal/pluginindex/index.json
GOFMT_PATHS = \
	cmd guardrails inference internal management plugin \
	tests/e2e tests/fidelity tests/fixtures/*.go tests/fixtures/fidelity tests/harness tests/integration tests/qualification

.PHONY: \
	help setup dev dev-stop dev-reset check check-go check-console check-contracts lint fmt-check vet \
	test test-unit test-unit-go test-unit-console test-integration test-e2e test-e2e-api test-e2e-browser \
	qualify-code qualify-code-live \
	api build build-go catalog catalog-sign fmt

help:
	@awk 'BEGIN {FS = ":.*## "} /^##@ / {printf "\n%s\n", substr($$0, 5)} /^[a-z0-9-]+:.*## / {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

##@ Everyday commands
setup: ## Install dependencies and the local watcher, then generate contracts
	@for tool in go node pnpm; do command -v "$$tool" >/dev/null || { printf 'Install %s, then rerun make setup\n' "$$tool" >&2; exit 1; }; done
	go mod download
	pnpm install --frozen-lockfile
	GOBIN="$(CURDIR)/.local/bin" go install github.com/air-verse/air@$(AIR_VERSION)
	$(MAKE) api

dev: ## Start services, automatically reloaded Go, and Vite
	./scripts/dev.sh

check: ## Check contracts, Go, console, and the unit tier without rewriting source
	$(MAKE) check-contracts
	$(MAKE) check-go
	$(MAKE) check-console
	$(MAKE) test-unit

api: ## Regenerate tracked contracts and ignored console types
	./scripts/api.sh
	pnpm --dir console api:generate

fmt: ## Format Go and console source
	gofmt -w $(GOFMT_PATHS)
	pnpm --dir console format

build: ## Build the native Go binary and static console from prepared contracts
	$(MAKE) build-go
	pnpm --dir console api:generate
	pnpm --dir console build

##@ Focused checks
check-go: ## Check Go formatting, vet, and static analysis
	$(MAKE) fmt-check vet lint

check-console: ## Prepare console types and check formatting, types, and lint
	pnpm --dir console api:generate
	pnpm --dir console format:check
	pnpm --dir console check

check-contracts: ## Check all tracked generated contracts without rewriting them
	./scripts/check-contracts.sh

##@ Tests
test: ## Run every tier, cheapest first
	$(MAKE) test-unit
	$(MAKE) test-integration
	$(MAKE) test-e2e

test-unit: ## Run both unit suites at once
	$(MAKE) -j2 --output-sync=target test-unit-go test-unit-console

test-unit-go: ## Run every untagged Go test inside a 30 second run budget
	./scripts/test-unit.sh go

test-unit-console: ## Run the console and script unit tests inside a 30 second run budget
	./scripts/test-unit.sh console

test-integration: ## Run Go code against real PostgreSQL and Valkey inside a 30 second run budget
	./scripts/test-integration.sh

test-e2e: ## Run both end to end suites
	$(MAKE) test-e2e-api
	$(MAKE) test-e2e-browser

test-e2e-api: ## Drive the olp binary over HTTP inside a 45 second run budget
	./scripts/test-e2e.sh api

test-e2e-browser: ## Drive the built console in Chromium inside a 60 second run budget
	./scripts/test-e2e.sh browser

qualify-code: ## Run the code mode fixture qualification lanes in disposable containers
	./scripts/qualification.sh

qualify-code-live: ## Run the code mode live qualification lanes against real vendor accounts
	./scripts/qualification-live.sh

fmt-check:
	@bad="$$(gofmt -l $(GOFMT_PATHS))"; test -z "$$bad" || { printf '%s\n' "$$bad"; exit 1; }

vet:
	go vet -tags integration,e2e ./...

lint:
	./scripts/check-lint.sh

##@ Services and maintenance
dev-stop: ## Stop development containers and preserve data
	docker compose -f deploy/compose.dev.yaml stop

dev-reset: ## Delete development containers and data after stopping make dev
	docker compose -f deploy/compose.dev.yaml down --volumes

build-go: ## Build the native Go binary from generated contracts
	mkdir -p -- "$$(dirname -- "$(GO_BUILD_OUTPUT)")"
	CGO_ENABLED=1 go build -mod=readonly -trimpath $(if $(strip $(GO_BUILD_TAGS)),-tags="$(GO_BUILD_TAGS)") -ldflags='$(LDFLAGS)' -o "$(GO_BUILD_OUTPUT)" ./cmd/olp

catalog: ## Rewrite the reference catalog and plugin index in canonical form
	go run ./internal/signing/cmd/olpsign fmt $(SIGNED_DOCUMENTS)

catalog-sign: catalog ## Sign the reference catalog and plugin index with the development key
	go run ./internal/signing/cmd/olpsign sign -key-id dev-2026a -seed-file internal/signing/devkey/dev-2026a.seed $(SIGNED_DOCUMENTS)
