//! PostgreSQL authority and cryptographic storage primitives for OpenLLMProxy.
//!
//! This crate deliberately owns SQL, encryption, and durable event delivery. It
//! does not expose SQLx types through the core ports.

pub mod access;
pub mod authentication;
pub mod circuits;
pub mod configuration;
mod error;
pub mod idempotency;
pub mod identity;
pub mod limits;
pub mod maintenance;
pub mod media_jobs;
pub mod oidc;
pub mod operations;
pub mod request_metadata;
pub mod runtime;
pub mod security;
mod store;
#[cfg(feature = "test-util")]
pub mod test_support;
pub mod usage;
pub mod valkey;
pub mod worker_health;

pub use error::PersistenceError;
pub use store::PgStore;

/// Truncates a query result fetched with `limit + 1` and derives the cursor
/// from the last visible item only when another page exists.
fn split_page<T, C>(
    mut items: Vec<T>,
    limit: usize,
    cursor: impl FnOnce(&T) -> C,
) -> (Vec<T>, Option<C>) {
    let has_more = items.len() > limit;
    items.truncate(limit);
    let next_cursor = if has_more {
        items.last().map(cursor)
    } else {
        None
    };
    (items, next_cursor)
}

/// SQLx embeds and checks every migration at compile time. Migrations execute
/// only in `migrate`/`all` mode, never implicitly in a gateway process.
pub static MIGRATOR: sqlx::migrate::Migrator = sqlx::migrate!("./migrations");
#[cfg(test)]
mod tests {
    use std::collections::BTreeSet;

    use super::split_page;

    #[test]
    fn migration_versions_are_unique() {
        let directory = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("migrations");
        let mut versions = BTreeSet::new();
        for entry in std::fs::read_dir(directory).unwrap() {
            let name = entry.unwrap().file_name().into_string().unwrap();
            if !name.ends_with(".sql") {
                continue;
            }
            let (version, _) = name
                .split_once('_')
                .unwrap_or_else(|| panic!("invalid migration filename: {name}"));
            let version = version
                .parse::<u32>()
                .unwrap_or_else(|_| panic!("invalid migration version: {name}"));
            assert!(
                versions.insert(version),
                "duplicate migration version: {name}"
            );
        }
        assert!(versions.contains(&32));
    }

    #[test]
    fn split_page_distinguishes_complete_and_overfetched_results() {
        assert_eq!(split_page(vec![1, 2], 3, |item| *item), (vec![1, 2], None));
        assert_eq!(split_page(vec![1, 2], 2, |item| *item), (vec![1, 2], None));
        assert_eq!(
            split_page(vec![1, 2, 3], 2, |item| *item),
            (vec![1, 2], Some(2))
        );
    }

    #[test]
    fn split_page_never_derives_a_cursor_without_a_visible_item() {
        assert_eq!(split_page(vec![1], 0, |item| *item), (Vec::new(), None));
    }
}
