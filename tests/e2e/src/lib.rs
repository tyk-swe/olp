//! Empty library anchor for the end-to-end journey harness.
//!
//! The real content lives in `tests/journey.rs`, which spawns the production
//! `olp` binary against real PostgreSQL, Valkey, and a loopback mock upstream
//! provider. Keeping this crate an empty `lib` with dev-only dependencies
//! keeps it outside the production dependency DAG checked by
//! `scripts/check-boundaries.sh`.
