# OpenLLMProxy task index. Every recipe is a thin dispatcher to the same
# script, cargo, or pnpm command CI runs (.github/workflows/ci.yml); keep the
# two in lockstep when either changes.

SHELL := bash
.SHELLFLAGS := -euo pipefail -c

FUZZ_TOOLCHAIN := nightly-2026-05-15
FUZZ_TARGETS := sse_decoder protocol_json media_metadata multipart_parser

.DEFAULT_GOAL := help

.PHONY: help check boundaries storage-sqlx fmt fmt-fix clippy test doctest \
	coverage console-install console-verify console-e2e console-storybook \
	screenshots openapi sqlx-prepare sqlx-check db-test release-version \
	supply-chain helm-verify script-selftest shellcheck fuzz-check \
	fuzz-replay sdk-smoke

help: ## List available targets
	@grep -E '^[a-z][a-z0-9-]*:.*##' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*##[ ]*"} {printf "  \033[1m%-18s\033[0m %s\n", $$1, $$2}'

check: boundaries storage-sqlx fmt clippy test console-verify release-version supply-chain ## Full PR gate (CI required tier, minus DB/image jobs)

boundaries: ## Enforce crate boundaries and dependency ownership (needs ripgrep)
	./scripts/check-boundaries.sh

storage-sqlx: ## Enforce typed storage access (no manual Row::get decoding)
	./scripts/check-storage-sqlx.sh

fmt: ## Check Rust formatting
	cargo fmt --all --check

fmt-fix: ## Apply Rust formatting
	cargo fmt --all

clippy: ## Clippy with -D warnings, offline sqlx metadata
	SQLX_OFFLINE=true cargo clippy --locked --workspace --all-targets --all-features -- -D warnings

test: ## Workspace unit tests (postgres-backed tests stay #[ignore]d; see db-test)
	SQLX_OFFLINE=true cargo test --locked --workspace --all-features

doctest: ## Workspace doctests (CI runs these separately from coverage)
	SQLX_OFFLINE=true cargo test --locked --workspace --all-features --doc

coverage: ## CI's real Rust test gate: llvm-cov nextest with the 51% line floor
	SQLX_OFFLINE=true cargo llvm-cov clean --workspace
	SQLX_OFFLINE=true cargo llvm-cov nextest --locked --workspace --all-features --no-report
	SQLX_OFFLINE=true cargo llvm-cov report --fail-under-lines 51

console-install: ## Install locked console dependencies
	pnpm --dir console install --frozen-lockfile

console-verify: ## Console gate: api:check + vitest + svelte-check/eslint + build
	pnpm --dir console verify

console-e2e: ## Console Playwright e2e suite
	pnpm --dir console test:e2e

console-storybook: ## Storybook interaction and accessibility tests
	pnpm --dir console test:storybook

screenshots: ## Regenerate docs/assets/screenshots/*.png from console fixtures
	pnpm --dir console screenshots

openapi: ## Regenerate openapi/management.json and the console API schema
	cargo run --locked -p olp --example export_openapi > openapi/management.json
	pnpm --dir console api:generate

sqlx-prepare: ## Regenerate .sqlx/ metadata against a migrated development database
	cargo sqlx prepare --workspace -- --all-targets --all-features

sqlx-check: ## Verify .sqlx/ metadata is fresh (CI: postgres-integration job)
	cargo sqlx prepare --workspace --check -- --all-targets --all-features

db-test: ## PostgreSQL/Valkey integration tests; needs OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX
	./scripts/run-postgres-tests.sh

release-version: ## Require consistent release metadata
	scripts/check-release-version.sh

supply-chain: ## Require immutable Action and image references
	scripts/check-supply-chain-pins.sh

helm-verify: ## Verify Helm values, schema, and templates change together
	scripts/verify-helm-contract.sh deploy/helm

script-selftest: ## Self-tests for the backup manifest and repository-validation helpers
	scripts/test-backup-manifest.sh
	scripts/test-repository-validation.sh

shellcheck: ## Shellcheck every tracked shell script
	git ls-files -z -- '*.sh' | xargs -0 shellcheck

fuzz-check: ## Compile fuzz targets (stable toolchain)
	cargo check --locked --manifest-path fuzz/Cargo.toml --bins

fuzz-replay: ## Replay fuzz regression corpora (needs the pinned nightly + cargo-fuzz)
	cd fuzz && for target in $(FUZZ_TARGETS); do \
		cargo +$(FUZZ_TOOLCHAIN) fuzz run "$$target" "corpus/$$target" -- -runs=0; \
	done

sdk-smoke: ## Official OpenAI/Anthropic/Gemini SDK smoke tests against a local build
	./tests/sdk-smoke/run.sh
