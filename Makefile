SHELL := bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help

.PHONY: help setup dev check test integration api build fmt

help: ## Show development commands
	@awk 'BEGIN {FS = ":.*## "} /^[a-z]+:.*## / {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

setup: ## Install dependencies and generate the API client
	pnpm install --frozen-lockfile
	$(MAKE) api

dev: ## Start local services, Rust, and Vite with hot reload
	./scripts/dev.sh

api: ## Generate OpenAPI and the TypeScript client
	mkdir -p openapi
	cargo run --locked --all-features --example export_openapi > openapi/management.json
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

build: api ## Build the release binary and console
	cargo build --locked --release
	pnpm --dir console build

fmt: ## Format Rust and console source
	cargo fmt --all
	pnpm --dir console format
