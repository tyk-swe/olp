SHELL := bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help

.PHONY: help setup dev check test integration api build fmt
.PHONY: go-setup go-dev go-api go-check go-test go-integration go-build go-fmt

help: ## Show development commands
	@awk 'BEGIN {FS = ":.*## "} /^[a-z-]+:.*## / {printf "  %-16s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

setup: ## Install dependencies and generate the API client
	pnpm install --frozen-lockfile
	$(MAKE) api

dev: ## Start local services, Rust, and Vite with hot reload
	./scripts/dev.sh

api: ## Generate OpenAPI and the TypeScript client
	mkdir -p openapi
	cargo run --locked --all-features --bin export_openapi > openapi/management.json
	pnpm --dir console api:generate

check: api ## Required PR check
	cargo fmt --all --check
	cargo clippy --locked --all-targets --all-features -- -D warnings
	$(MAKE) test
	pnpm --dir console verify

test: ## Run Rust unit and protocol tests
	cargo test --locked --all-features

integration: api ## Run service integration and Chromium journeys
	./scripts/integration.sh

build: ## Build the release binary and console
	cargo build --locked --release --bin olp --bin export_openapi
	mkdir -p openapi
	source scripts/lib/cargo-target-dir.sh; \
		"$$(cargo_target_dir "$(CURDIR)")/release/export_openapi" > openapi/management.json
	pnpm --dir console api:generate
	pnpm --dir console build

fmt: ## Format Rust and console source
	cargo fmt --all
	pnpm --dir console format

go-setup: ## Install pinned Go/console dependencies and generate contracts
	go mod download
	pnpm install --frozen-lockfile
	$(MAKE) go-api

go-dev: ## Start isolated Go services, gateway, and Vite hot reload
	./scripts/go-dev.sh

go-api: ## Generate Go and TypeScript contracts without services or Rust
	./scripts/go-api.sh

go-check: go-api ## Check Go formatting, vet, behavior, and the existing console
	@test -z "$$(gofmt -l cmd internal openapi/*.go tests/fixtures/*.go tests/integration tests/sdkfixture)" || { gofmt -l cmd internal openapi/*.go tests/fixtures/*.go tests/integration tests/sdkfixture; exit 1; }
	go vet ./...
	$(MAKE) go-test
	pnpm --dir console verify

go-test: ## Run Go unit/protocol tests without containers
	go test ./...

go-integration: ## Run isolated PostgreSQL/GLIDE, process, SDK, and browser checks
	./scripts/go-integration.sh

go-build: go-api ## Build the native Go binary and static console separately
	mkdir -p .local/bin
	CGO_ENABLED=1 go build -trimpath -o .local/bin/olp ./cmd/olp
	pnpm --dir console build

go-fmt: ## Format Go and the console
	gofmt -w cmd internal openapi/*.go tests/fixtures/*.go tests/integration tests/sdkfixture
	pnpm --dir console format
