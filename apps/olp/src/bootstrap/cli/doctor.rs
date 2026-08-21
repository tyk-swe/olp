use std::path::Path;

use olp_db::limits::DistributedLimiter;
use serde_json::json;

use crate::{
    bootstrap::connectors::register_mounted_connectors, bootstrap::media_spool,
    bootstrap::state::TransportRegistry,
};

use super::{
    AppResult,
    config::DoctorArgs,
    validation::{check_secret_permissions, connect_store, load_auth_hmac_key, load_master_key},
};

pub(super) async fn doctor(args: DoctorArgs) -> AppResult<()> {
    let mut checks = serde_json::Map::new();
    let store = connect_store(&args.persistence.database).await?;
    store.ping().await?;
    checks.insert("postgresql".into(), json!({ "ok": true }));

    let keyspace = store.valkey_keyspace().await?;
    let limiter = DistributedLimiter::connect(
        &args.persistence.valkey_url,
        &format!("{}:doctor", keyspace.prefix()),
    )
    .await?;
    limiter.ping().await?;
    checks.insert("valkey".into(), json!({ "ok": true }));
    olp_db::valkey::verify_request_metadata_stream_upgrade(&args.persistence.valkey_url).await?;
    checks.insert(
        "request_metadata_stream_upgrade".into(),
        json!({ "ok": true }),
    );

    load_auth_hmac_key(&args.auth_hmac_key_file).await?;
    load_master_key(&args.master_key_file).await?;
    check_secret_permissions(&args.auth_hmac_key_file).await?;
    check_secret_permissions(&args.master_key_file).await?;
    checks.insert("secret_files".into(), json!({ "ok": true }));

    if let Some(path) = &args.assets.connector_config_file {
        let registry = TransportRegistry::default();
        register_mounted_connectors(path, &registry).await?;
        checks.insert(
            "connector_config".into(),
            json!({ "ok": true, "configured": registry.snapshot().len() }),
        );
    }

    if !args.assets.console_dir.join("index.html").is_file() {
        return Err(std::io::Error::other(format!(
            "console index is missing at {}",
            args.assets.console_dir.join("index.html").display()
        ))
        .into());
    }
    checks.insert("console".into(), json!({ "ok": true }));
    let media_spool_dir = args
        .assets
        .media_spool_dir
        .as_deref()
        .map_or_else(std::env::temp_dir, Path::to_path_buf);
    let media_spool =
        media_spool::create(&media_spool_dir, args.assets.media_spool_capacity_bytes)?;
    drop(media_spool);
    checks.insert(
        "media_spool".into(),
        json!({
            "ok": true,
            "capacity_bytes": args.assets.media_spool_capacity_bytes,
        }),
    );
    println!(
        "{}",
        serde_json::to_string_pretty(&json!({ "ok": true, "checks": checks }))?
    );
    Ok(())
}
