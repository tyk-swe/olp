use std::path::Path;

use crate::limits::distributed::DistributedLimiter;
use serde_json::json;

use crate::media::spool as media_spool;
use crate::providers::mounted::register_mounted_connectors;
use crate::runtime::transports::TransportRegistry;

use crate::crypto::secret_files::check_secret_permissions;
use crate::process::cli::AppResult;
use crate::process::cli::config::DoctorArgs;
use crate::process::cli::validation::connect_database;
use crate::process::cli::validation::load_auth_hmac_key;
use crate::process::cli::validation::load_master_key;

pub(crate) async fn doctor(args: DoctorArgs) -> AppResult<()> {
    let mut checks = serde_json::Map::new();
    let pool = connect_database(&args.persistence.database).await?;
    crate::database::ping(&pool).await?;
    checks.insert("postgresql".into(), json!({ "ok": true }));

    let keyspace = crate::limits::valkey::valkey_keyspace(&pool).await?;
    let limiter = DistributedLimiter::connect(
        &args.persistence.valkey_url,
        &format!("{}:doctor", keyspace.prefix()),
    )
    .await?;
    limiter.ping().await?;
    checks.insert("valkey".into(), json!({ "ok": true }));

    load_auth_hmac_key(&args.auth_hmac_key_file).await?;
    load_master_key(&args.master_key_file).await?;
    check_secret_permissions(&args.auth_hmac_key_file).await?;
    check_secret_permissions(&args.master_key_file).await?;
    checks.insert("secret_files".into(), json!({ "ok": true }));

    if let Some(path) = &args.assets.connector_config_file {
        let registry = TransportRegistry::default();
        register_mounted_connectors(
            path,
            &registry,
            &args.provider_egress.policy(),
            crate::providers::connector::ResponseLimits::default(),
        )
        .await?;
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
