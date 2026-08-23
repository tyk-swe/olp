use base64::Engine as _;
use sha2::{Digest, Sha256};
use tracing::info;

use super::{AppResult, config::MigrateArgs, validation::connect_store};

const DATABASE_IDENTITY_QUERY_PARAMS: &[&str] = &["dbname", "host", "hostaddr", "port"];

pub(super) async fn migrate(args: MigrateArgs) -> AppResult<()> {
    let store = connect_store(&args.persistence.database).await?;
    if let Some(target) = args.through_version {
        if std::env::var("OLP_ALLOW_PARTIAL_MIGRATIONS_FOR_TESTS").as_deref() != Ok("test-only") {
            return Err(std::io::Error::other(
                "partial migration targets are restricted to test fixtures",
            )
            .into());
        }
        store.migrate_to(target).await?;
        info!(target, "PostgreSQL migrations reached test target");
    } else {
        let legacy_stream_claim_token =
            legacy_request_metadata_stream_claim_token(&args.persistence.database.database_url)?;
        let should_claim_legacy_stream =
            store.should_claim_legacy_request_metadata_stream().await?;
        let legacy_stream_claim_prepared = if should_claim_legacy_stream {
            olp_db::valkey::mark_legacy_request_metadata_stream_claim(
                &args.persistence.valkey_url,
                &legacy_stream_claim_token,
            )
            .await?
        } else {
            false
        };
        store.migrate().await?;
        info!("PostgreSQL migrations are current");
        let keyspace = store.valkey_keyspace().await?;
        let migrated = olp_db::valkey::migrate_claimed_legacy_request_metadata_stream(
            &args.persistence.valkey_url,
            &keyspace.request_metadata_stream(),
            &legacy_stream_claim_token,
        )
        .await?;
        if migrated || legacy_stream_claim_prepared {
            info!(
                migrated,
                stream = %keyspace.request_metadata_stream(),
                "legacy request metadata stream transition is complete"
            );
        } else {
            olp_db::valkey::verify_request_metadata_stream_upgrade(&args.persistence.valkey_url)
                .await?;
            info!("legacy request metadata stream claim skipped for non-upgrade database");
        }
    }
    Ok(())
}

pub(super) fn legacy_request_metadata_stream_claim_token(database_url: &str) -> AppResult<String> {
    let mut identity_url = url::Url::parse(database_url).map_err(|error| {
        std::io::Error::other(format!(
            "invalid database URL for legacy request metadata stream claim: {error}"
        ))
    })?;
    identity_url.set_username("").map_err(|()| {
        std::io::Error::other("database URL cannot be normalized for legacy stream claim")
    })?;
    identity_url.set_password(None).map_err(|()| {
        std::io::Error::other("database URL cannot be normalized for legacy stream claim")
    })?;
    let mut identity_query_params = identity_url
        .query_pairs()
        .filter(|(key, _)| DATABASE_IDENTITY_QUERY_PARAMS.contains(&key.as_ref()))
        .map(|(key, value)| (key.into_owned(), value.into_owned()))
        .collect::<Vec<_>>();
    identity_query_params.sort_unstable();
    identity_url.set_query(None);
    if !identity_query_params.is_empty() {
        let mut query_pairs = identity_url.query_pairs_mut();
        for (key, value) in identity_query_params {
            query_pairs.append_pair(&key, &value);
        }
    }
    identity_url.set_fragment(None);
    let digest = Sha256::digest(identity_url.as_str().as_bytes());
    Ok(format!(
        "database-url-sha256-v1:{}",
        base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(digest)
    ))
}
