# OpenLLMProxy task index. Every recipe is a thin dispatcher to the same
# script, cargo, or pnpm command CI runs (.github/workflows/ci.yml); keep the
# two in lockstep when either changes.

SHELL := bash
.SHELLFLAGS := -euo pipefail -c

FUZZ_TOOLCHAIN := nightly-2026-05-15
# Derived from fuzz/Cargo.toml rather than restated here, so a newly added
# [[bin]] target cannot be silently skipped by the replay and campaign recipes.
FUZZ_TARGETS := $(shell awk '/^\[\[bin\]\]/ { in_bin = 1; next } in_bin && $$1 == "name" { gsub(/"/, "", $$3); print $$3; in_bin = 0 }' fuzz/Cargo.toml)
# cargo-fuzz defaults `--target` to the triple it was itself built for, and the
# prebuilt binaries cargo-binstall serves are musl. A sanitizer build cannot
# link against a static libc, so always fuzz for the host triple instead.
FUZZ_TRIPLE = $(shell rustc -vV | sed -n 's/^host: //p')

.DEFAULT_GOAL := help

.PHONY: help check boundaries storage-sqlx source-size fmt fmt-fix clippy test \
	coverage console-install console-verify console-e2e \
	screenshots openapi sqlx-prepare sqlx-check db-test release-version \
	supply-chain helm-verify script-selftest shellcheck fuzz-check \
	fuzz-replay fuzz-campaign sdk-smoke sdk-smoke-install sdk-smoke-run \
	e2e worker-ha smoke-image-modes upgrade-rehearsal advisories deny

help: ## List available targets
	@grep -E '^[a-z][a-z0-9-]*:.*##' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*##[ ]*"} {printf "  \033[1m%-18s\033[0m %s\n", $$1, $$2}'

# Every required-tier gate that needs only the standard toolchain. CI
# additionally enforces the coverage floor (make coverage), the DB/Valkey
# suites (make db-test) and the SQLx metadata check (make sqlx-check), the
# fuzz replay (make fuzz-replay) and bounded campaign (make fuzz-campaign),
# sdk-smoke, the contract suites (make e2e, make worker-ha, plus the
# toxiproxy-backed ha job), the console Playwright and console-integration
# jobs, the upgrade rehearsal (make upgrade-rehearsal), the amd64 and arm64
# image builds (make smoke-image-modes), helm-verify (needs helm + docker
# compose), the advisory audits (make deny, make advisories), and the
# actionlint/hadolint quality steps.
check: boundaries storage-sqlx source-size shellcheck script-selftest fmt clippy test console-verify release-version supply-chain ## Broad local gate; CI also runs service/tool-specific required jobs

boundaries: ## Enforce crate boundaries and dependency ownership (needs ripgrep)
	./scripts/check-boundaries.sh

storage-sqlx: ## Enforce typed storage access (no manual Row::get decoding)
	./scripts/check-storage-sqlx.sh

source-size: ## Enforce AGENTS.md size rules (files <30 KB, fns <100 lines); baseline only shrinks
	./scripts/check-source-size.sh

fmt: ## Check Rust formatting
	cargo fmt --all --check

fmt-fix: ## Apply Rust formatting
	cargo fmt --all

clippy: ## Clippy with -D warnings, offline sqlx metadata
	SQLX_OFFLINE=true cargo clippy --locked --workspace --all-targets --all-features -- -D warnings

# The workspace deliberately has zero doctests; nextest and llvm-cov do not
# run them. If you add one, restore a `cargo test --doc` gate here and in CI.
test: ## Workspace unit tests via nextest (postgres-backed tests stay #[ignore]d; see db-test)
	SQLX_OFFLINE=true cargo nextest run --locked --workspace --all-features

# test_support only executes under `make db-test`, which coverage never
# runs; llvm-cov's defaults already exclude tests/ dirs and src tests.rs
# modules from the report.
coverage: ## CI's real Rust test gate: llvm-cov nextest with the 62% line floor
	SQLX_OFFLINE=true NEXTEST_PROFILE=ci cargo llvm-cov nextest --locked --workspace --all-features \
		--ignore-filename-regex 'src/test_support\.rs' \
		--lcov --output-path lcov.info --fail-under-lines 62

console-install: ## Install locked console dependencies
	pnpm --dir console install --frozen-lockfile

console-verify: ## Console gate: api:check + vitest + svelte-check/eslint + build
	pnpm --dir console verify

console-e2e: ## Console Playwright e2e suite: all four projects; CI splits chromium from firefox/webkit/mobile-chromium
	pnpm --dir console test:e2e

screenshots: ## Regenerate docs/assets/screenshots/*.png from console fixtures
	pnpm --dir console screenshots

openapi: ## Regenerate openapi/management.json and the console API schema
	cargo run --locked -p olp --example export_openapi > openapi/management.json
	pnpm --dir console api:generate

sqlx-prepare: ## Regenerate .sqlx/ metadata against a migrated development database
	cargo sqlx prepare --workspace -- --all-targets --all-features

sqlx-check: ## Verify .sqlx/ metadata is fresh (CI: postgres-integration job)
	cargo sqlx prepare --workspace --check -- --all-targets --all-features

db-test: ## PostgreSQL/Valkey integration tests via nextest; needs OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX; extra args via ARGS
	./scripts/run-postgres-tests.sh $(ARGS)

e2e: ## End-to-end contract suite: real olp binary + PostgreSQL + Valkey + mock upstream; any contract violation fails
	./scripts/run-e2e-tests.sh $(ARGS)

worker-ha: ## Shared-Valkey isolation and three-worker crash recovery against real services
	OLP_E2E_TEST_TARGET=worker-ha ./scripts/run-e2e-tests.sh $(ARGS)

release-version: ## Require consistent release metadata
	scripts/check-release-version.sh

supply-chain: ## Require immutable Action and image references
	scripts/check-supply-chain-pins.sh

helm-verify: ## Verify Helm values, schema, and templates change together
	scripts/verify-helm-contract.sh deploy/helm

script-selftest: ## Self-tests for shell helpers and repository invariants
	scripts/test-backup-manifest.sh
	scripts/test-postgres-test-databases.sh
	scripts/test-repository-validation.sh
	scripts/test-check-source-size.sh
	scripts/test-record-request-metadata-stream-loss.sh

shellcheck: ## Shellcheck every tracked or untracked repository shell script
	@scripts=(); \
	while IFS= read -r -d '' script; do [[ ! -f $$script ]] || scripts+=("$$script"); done < <(git ls-files --cached --others --exclude-standard -z -- '*.sh'); \
	if (( $${#scripts[@]} == 0 )); then echo "no tracked shell scripts were found" >&2; exit 1; fi; \
	shellcheck "$${scripts[@]}" && echo "shellcheck passed"

fuzz-check: ## Compile fuzz targets (stable toolchain)
	cargo check --locked --manifest-path fuzz/Cargo.toml --bins

fuzz-replay: ## Replay fuzz regression corpora (installs the pinned nightly; needs cargo-fuzz)
	rustup toolchain install $(FUZZ_TOOLCHAIN) --profile minimal
	cd fuzz && for target in $(FUZZ_TARGETS); do \
		cargo +$(FUZZ_TOOLCHAIN) fuzz run --target $(FUZZ_TRIPLE) "$$target" "corpus/$$target" -- -runs=0; \
	done

FUZZ_MAX_TOTAL_TIME ?= 120
fuzz-campaign: ## Bounded fuzz campaign: each seeded target for FUZZ_MAX_TOTAL_TIME seconds
	rustup toolchain install $(FUZZ_TOOLCHAIN) --profile minimal
	cd fuzz && for target in $(FUZZ_TARGETS); do \
		cargo +$(FUZZ_TOOLCHAIN) fuzz run --target $(FUZZ_TRIPLE) "$$target" "corpus/$$target" -- -max_total_time=$(FUZZ_MAX_TOTAL_TIME); \
	done

sdk-smoke-install: ## Install locked SDK smoke-test dependencies
	pnpm --dir tests/sdk-smoke install --frozen-lockfile

sdk-smoke: sdk-smoke-install ## Official OpenAI/Anthropic/Gemini SDK smoke tests against a local build
	$(MAKE) sdk-smoke-run

# CI installs these dependencies through setup-node-pnpm, so its job runs this
# target instead of sdk-smoke and skips the second pnpm install.
sdk-smoke-run: ## Run the SDK smoke tests against already-installed dependencies
	./tests/sdk-smoke/run.sh

IMAGE ?= openllmproxy:ci-amd64
smoke-image-modes: ## Smoke a built image's binary modes, non-root user, and packaged console; set IMAGE and OLP_IMAGE_PLATFORM
	./scripts/smoke-image-modes.sh $(IMAGE)

upgrade-rehearsal: ## Backup/restore/upgrade rehearsal; needs OLP_REHEARSAL_SERVER_URL, OLP_DATABASE_URL, OLP_VALKEY_URL, OLP_BIN
	./scripts/ci/run-upgrade-rehearsal.sh

deny: ## Cargo advisories, bans, licenses, and sources, all configured in deny.toml
	cargo deny check advisories bans licenses sources

advisories: ## Console and SDK smoke dependency advisories (Cargo advisories run under make deny)
	pnpm --dir console audit --audit-level high
	pnpm --dir tests/sdk-smoke audit --prod --audit-level high
