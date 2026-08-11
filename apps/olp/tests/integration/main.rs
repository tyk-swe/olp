#![cfg(feature = "test-util")]

//! Single integration-test binary for the olp app. The `*_postgres`
//! suites stay `#[ignore]`d and run via `make db-test`
//! (`scripts/run-postgres-tests.sh`) with one database per test from
//! `olp_db::test_support`; the other modules run in every `make test`.
//! One binary instead of seven keeps link time and target size in check.

mod common;

mod anthropic_gemini_inference;
mod configuration_http_postgres;
mod identity_http_postgres;
mod media_jobs_http_postgres;
mod oidc_http_postgres;
mod openapi_drift;
mod operations_http_postgres;
mod runtime_outbox_postgres;
