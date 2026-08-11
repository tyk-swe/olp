//! Single integration-test binary for olp-db. The PostgreSQL and
//! Valkey suites stay `#[ignore]`d here and run via `make db-test`
//! (`scripts/run-postgres-tests.sh`), which executes them through nextest
//! with one database per test provisioned by `olp_db::test_support`.
//! One binary instead of eighteen keeps link time and target size in check.

mod support;

mod configuration_postgres;
mod distributed_limits_valkey;
mod idempotency_replay_postgres;
mod identity_postgres;
mod management_idempotency_replay_postgres;
mod master_key_reencryption_postgres;
mod media_jobs_postgres;
mod oidc_flow_postgres;
mod operations_postgres;
mod provider_revisions_postgres;
mod request_metadata_consumer_health_postgres;
#[cfg(all(feature = "test-util", debug_assertions))]
mod request_metadata_consumer_valkey;
mod request_metadata_naming_upgrade_postgres;
mod route_activation_revalidation_postgres;
mod route_draft_simulation_postgres;
mod runtime_fallback_postgres;
mod runtime_publication_postgres;
mod upgrade_0021_postgres;
mod usage_surface_upgrade_postgres;
mod worker_ha_postgres;
