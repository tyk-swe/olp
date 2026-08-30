# OpenLLMProxy task index. Every recipe is a thin dispatcher to the same
# script, cargo, or pnpm command CI runs (.github/workflows/ci.yml); `make
# ci-lockstep` fails if a ci.yml step bypasses this file.

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

.PHONY: help check check-static check-cargo check-heavy boundaries storage-sqlx source-size fmt fmt-fix clippy test \
	coverage coverage-unit coverage-db coverage-report console-install console-verify console-e2e \
	screenshots openapi sqlx-prepare sqlx-check db-test release-version release-verify \
	release-image release-manifest release-chart release-notes \
	supply-chain machete ci-lockstep helm-verify script-selftest shellcheck fuzz-check \
	olp-build-test-util olp-prebuilt olp-migrate sqlx-migrate playwright-install \
	console-e2e-project console-integration-prebuilt \
	fuzz-replay fuzz-campaign live-tests sdk-smoke sdk-smoke-install sdk-smoke-run \
	sdk-smoke-python sdk-smoke-python-install \
	e2e worker-ha smoke-image-modes upgrade-rehearsal advisories deny

help: ## List available targets
	@grep -E '^[a-z][a-z0-9-]*:.*##' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*##[ ]*"} {printf "  \033[1m%-18s\033[0m %s\n", $$1, $$2}'

# Every required-tier gate that needs only the standard toolchain, in two
# tiers so cheap failures surface before anything compiles:
#   check-static  every script/format gate (~10 s), run in parallel
#   check-heavy   clippy -> nextest serially (they share the cargo lock and
#                 target dir) alongside console-verify, CHECK_JOBS at a time
# Leaf targets are unchanged; CI invokes them individually. Required CI also
# enforces the coverage floor (make coverage, including the DB/Valkey suites;
# it needs the make db-test environment), the SQLx metadata check (make
# sqlx-check), stable fuzz compilation (make fuzz-check), sdk-smoke, the e2e
# contract suite, required console Playwright and console-integration jobs,
# the amd64 image build (make smoke-image-modes), helm-verify (needs helm +
# docker compose), advisory audits (make deny, make advisories), and the
# actionlint/hadolint quality steps. Full CI adds the nightly fuzz replay and
# bounded campaign (make fuzz-replay, make fuzz-campaign), worker-ha, the
# toxiproxy-backed ha job, cross-browser Playwright, upgrade rehearsal (make
# upgrade-rehearsal), and the arm64 image build. The harness jobs share one
# `olp --features test-util` binary built by the rust-test-util-build job;
# locally the scripts build it on demand.
CHECK_JOBS ?= 2
STATIC_JOBS ?= $(shell nproc 2>/dev/null || echo 4)

check: ## Broad local gate: check-static, then check-heavy; CI also runs service/tool-specific required jobs
	$(MAKE) -j$(STATIC_JOBS) --output-sync=target check-static
	$(MAKE) -j$(CHECK_JOBS) --output-sync=recurse check-heavy

check-static: boundaries storage-sqlx source-size shellcheck script-selftest fmt release-version supply-chain machete ci-lockstep ## Cheap script and formatting gates only (parallel-safe; quick pre-commit loop)

check-cargo: ## Clippy then the nextest suite, serially (shared cargo lock)
	$(MAKE) clippy
	$(MAKE) test

check-heavy: check-cargo console-verify ## The compile-heavy gates; run with -j to overlap cargo and pnpm

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

coverage: ## CI's Rust gate: unit and DB suites with an 80% line floor; needs the db-test environment
	cargo llvm-cov clean --workspace
	$(MAKE) coverage-unit
	$(MAKE) coverage-db
	$(MAKE) coverage-report

coverage-unit: ## Collect coverage from the workspace unit suites without reporting
	SQLX_OFFLINE=true NEXTEST_PROFILE=ci cargo llvm-cov nextest --no-report --locked --workspace --all-features

coverage-db: ## Add the ignored PostgreSQL/Valkey suites to the coverage data
	OLP_DB_TEST_RUNNER="cargo llvm-cov nextest --no-report" ./scripts/run-postgres-tests.sh

coverage-report: ## Print coverage and write lcov.info, enforcing the 80% line floor
	cargo llvm-cov report --summary-only
	cargo llvm-cov report --lcov --output-path lcov.info \
		--ignore-filename-regex 'src/test_support\.rs' --fail-under-lines 80

console-install: ## Install locked console dependencies
	pnpm --dir console install --frozen-lockfile

console-verify: ## Console gate: api:check + vitest + svelte-check/eslint + build
	pnpm --dir console verify

console-e2e: ## Console Playwright e2e suite: all four projects; CI runs them one project per job via console-e2e-project
	pnpm --dir console test:e2e

BROWSER ?= chromium
PROJECT ?= chromium

playwright-install: ## Install one Playwright browser with its system packages (BROWSER=chromium)
	pnpm --dir console exec playwright install --with-deps $(BROWSER)

# svelte-kit sync first: a job that downloads the prebuilt console never ran
# a build, so .svelte-kit/tsconfig.json (which console/tsconfig.json extends)
# does not exist yet and Playwright refuses to load the tests without it.
console-e2e-project: ## Console Playwright e2e for one project (PROJECT=chromium)
	pnpm --dir console exec svelte-kit sync
	pnpm --dir console exec playwright test --project=$(PROJECT)

console-integration-prebuilt: ## Rust-hosted console integration against the prebuilt console and olp binary
	scripts/ci/run-console-integration.sh

screenshots: ## Regenerate docs/assets/screenshots/*.png from console fixtures
	pnpm --dir console screenshots

openapi: ## Regenerate openapi/management.json and the console API schema
	cargo run --locked -p olp --example export_openapi > openapi/management.json
	pnpm --dir console api:generate

sqlx-prepare: ## Regenerate .sqlx/ metadata against a migrated development database
	cargo sqlx prepare --workspace -- --all-targets --all-features

sqlx-migrate: ## Apply the migrations to DATABASE_URL with sqlx-cli
	cargo sqlx migrate run --source crates/olp-db/migrations

sqlx-check: ## Verify .sqlx/ metadata is fresh (CI: postgres-integration job)
	cargo sqlx prepare --workspace --check -- --all-targets --all-features

olp-build-test-util: ## Build the harness olp binary that the contract and console jobs share
	SQLX_OFFLINE=true cargo build --locked -p olp --features test-util

olp-prebuilt: ## Restore the execute bit an artifact download drops from target/debug/olp
	chmod +x target/debug/olp

olp-migrate: ## Apply the migrations with the built olp binary
	target/debug/olp migrate

db-test: ## PostgreSQL/Valkey integration tests via nextest; needs OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX; extra args via ARGS
	./scripts/run-postgres-tests.sh $(ARGS)

e2e: ## End-to-end contract suite: real olp binary + PostgreSQL + Valkey + mock upstream; any contract violation fails
	./scripts/run-e2e-tests.sh $(ARGS)

worker-ha: ## Shared-Valkey isolation and three-worker crash recovery against real services
	OLP_E2E_TEST_TARGET=worker-ha ./scripts/run-e2e-tests.sh $(ARGS)

release-version: ## Require consistent release metadata
	scripts/check-release-version.sh

release-verify: ## Require a strict release tag matching every version pin
	scripts/check-release-tag.sh

release-image: ## Record, smoke, or attest one release image; set RELEASE_IMAGE_STEP
	scripts/release-image.sh

release-manifest: ## Create or sign the multi-architecture release index; set RELEASE_MANIFEST_STEP
	scripts/release-manifest.sh

release-chart: ## Package and optionally publish the release Helm chart
	scripts/release-chart.sh

release-notes: ## Extract release notes, checksum assets, and optionally create the GitHub Release
	scripts/release-notes.sh

supply-chain: ## Require immutable Action and image references
	scripts/check-supply-chain-pins.sh

machete: ## Reject workspace dependencies no crate uses (needs cargo-machete)
	cargo machete --with-metadata

ci-lockstep: ## Require every ci.yml command step to run through make or scripts/ci
	scripts/check-ci-make-lockstep.sh

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

live-tests: ## Run the credentialed live-provider drift tests
	SQLX_OFFLINE=true cargo nextest run --locked -p olp-engine --all-features \
		--profile live --run-ignored only -E 'test(/live_provider/)'

sdk-smoke-install: ## Install locked SDK smoke-test dependencies
	pnpm --dir tests/sdk-smoke install --frozen-lockfile

sdk-smoke: sdk-smoke-install ## Official OpenAI/Anthropic/Gemini SDK smoke tests against a local build
	$(MAKE) sdk-smoke-run

# CI installs these dependencies through setup-node-pnpm, so its job runs this
# target instead of sdk-smoke and skips the second pnpm install.
sdk-smoke-run: ## Run the SDK smoke tests against already-installed dependencies
	./tests/sdk-smoke/run.sh

sdk-smoke-python-install: ## Install locked Python SDK smoke-test dependencies
	uv sync --project tests/sdk-smoke-python --locked --no-dev

sdk-smoke-python: sdk-smoke-python-install ## Run the official Python SDK smoke tests
	./tests/sdk-smoke/run.sh uv run --project tests/sdk-smoke-python --locked --no-dev \
		python tests/sdk-smoke-python/smoke.py

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
	uv audit --project tests/sdk-smoke-python --locked --no-dev
