use std::{path::Path, time::Duration};

use olp_storage::{
    DistributedLimiter, MasterKey, MasterKeyEncryptionStatus, PgStore, RuntimeHintPublisher,
    ValkeyAdapterError, preflight_request_metadata_stream_upgrade, run_request_metadata_consumer,
};
use serde_json::json;
use tokio::{sync::watch, task::JoinSet};
use tracing::{error, info, warn};

use crate::{TransportRegistry, connectors::load_connector_config, create_media_spool};

use super::{
    AppResult, BACKGROUND_SHUTDOWN_TIMEOUT,
    config::{
        BackendArgs, DoctorArgs, InternalPreStopArgs, MasterKeyAction, MasterKeyArgs, MigrateArgs,
    },
    startup::shutdown_signal,
    validation::{
        check_secret_permissions, connect_store, ensure_keyring_covers_references,
        load_auth_hmac_key, load_master_key,
    },
};

pub(super) async fn internal_pre_stop(args: InternalPreStopArgs) -> AppResult<()> {
    tokio::time::sleep(Duration::from_secs(args.seconds)).await;
    Ok(())
}

pub(super) async fn migrate(args: MigrateArgs) -> AppResult<()> {
    preflight_request_metadata_stream_upgrade(&args.backend.valkey_url).await?;
    let store = connect_store(&args.backend.database).await?;
    if let Some(target) = args.through_version {
        if std::env::var("OLP_ALLOW_PARTIAL_MIGRATIONS_FOR_TESTS").as_deref() != Ok("test-only") {
            return Err(std::io::Error::other(
                "partial migration targets are restricted to test fixtures",
            )
            .into());
        }
        olp_storage::MIGRATOR.run_to(target, store.pool()).await?;
        info!(target, "PostgreSQL migrations reached test target");
    } else {
        store.migrate().await?;
        info!("PostgreSQL migrations are current");
    }
    Ok(())
}

pub(super) async fn run_worker_command(args: BackendArgs) -> AppResult<()> {
    let store = connect_store(&args.database).await?;
    preflight_request_metadata_stream_or_defer(&args.valkey_url).await?;
    let (sender, receiver) = watch::channel(false);
    let mut workers = JoinSet::new();
    spawn_worker_supervisors(&mut workers, store, args.valkey_url, receiver);
    let early_exit = tokio::select! {
        result = workers.join_next() => Some(result),
        () = shutdown_signal() => None,
    };
    let _ = sender.send(true);
    let stop_result = stop_worker_tasks(&mut workers, BACKGROUND_SHUTDOWN_TIMEOUT).await;
    match (early_exit, stop_result) {
        (Some(Some(Err(error))), _) | (_, Err(error)) => Err(error.into()),
        (None, Ok(())) => Ok(()),
        (Some(Some(Ok(()))) | Some(None), Ok(())) => {
            Err(std::io::Error::other("worker supervisor stopped unexpectedly").into())
        }
    }
}

pub(super) async fn preflight_request_metadata_stream_or_defer(valkey_url: &str) -> AppResult<()> {
    match preflight_request_metadata_stream_upgrade(valkey_url).await {
        Ok(()) => Ok(()),
        Err(
            error @ (ValkeyAdapterError::LegacyRequestMetadataStreamNotDrained { .. }
            | ValkeyAdapterError::LegacyRequestMetadataStreamAcknowledgedEntries { .. }),
        ) => Err(error.into()),
        Err(error) => {
            warn!(%error, "request metadata upgrade preflight deferred until Valkey reconnects");
            Ok(())
        }
    }
}

pub(super) async fn stop_worker_tasks(
    workers: &mut JoinSet<()>,
    timeout: Duration,
) -> Result<(), tokio::task::JoinError> {
    let deadline = tokio::time::Instant::now() + timeout;
    let mut first_error = None;
    loop {
        match tokio::time::timeout_at(deadline, workers.join_next()).await {
            Ok(Some(Ok(()))) => {}
            Ok(Some(Err(error))) if error.is_cancelled() => {}
            Ok(Some(Err(error))) => {
                warn!(%error, "worker task stopped unexpectedly");
                if first_error.is_none() {
                    first_error = Some(error);
                }
            }
            Ok(None) => return first_error.map_or(Ok(()), Err),
            Err(_) => {
                warn!("worker tasks did not stop before deadline; aborting them");
                workers.abort_all();
                while let Some(result) = workers.join_next().await {
                    if let Err(error) = result
                        && !error.is_cancelled()
                    {
                        warn!(%error, "worker task failed while stopping");
                        if first_error.is_none() {
                            first_error = Some(error);
                        }
                    }
                }
                return first_error.map_or(Ok(()), Err);
            }
        }
    }
}

fn spawn_worker_supervisors(
    workers: &mut JoinSet<()>,
    store: PgStore,
    valkey_url: String,
    shutdown: watch::Receiver<bool>,
) {
    workers.spawn(outbox_supervisor(
        store.clone(),
        valkey_url.clone(),
        shutdown.clone(),
    ));
    workers.spawn(request_metadata_consumer_supervisor(
        store.clone(),
        valkey_url,
        shutdown.clone(),
    ));
    workers.spawn(maintenance_supervisor(store.clone(), shutdown.clone()));
    workers.spawn(request_metadata_epoch_supervisor(store, shutdown));
}

pub(super) async fn maintenance_supervisor(store: PgStore, mut shutdown: watch::Receiver<bool>) {
    // Frequent bounded passes keep receipt expiry from becoming one large
    // hourly DELETE/WAL spike at qualified request rates.
    let mut interval = tokio::time::interval(Duration::from_secs(60));
    interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! {
            _ = interval.tick() => {
                match store.run_maintenance(chrono::Utc::now()).await {
                    Ok(report) => info!(?report, "maintenance pass completed"),
                    Err(error) => error!(%error, "maintenance pass failed; retrying next interval"),
                }
            }
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    return;
                }
            }
        }
    }
}

pub(super) async fn request_metadata_epoch_supervisor(
    store: PgStore,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut interval = tokio::time::interval(Duration::from_secs(5));
    interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! {
            _ = interval.tick() => {
                match store.detect_stale_request_metadata_gateway_epochs(chrono::Utc::now()).await {
                    Ok(report) if report.detected_epochs > 0 => warn!(
                        detected_epochs = report.detected_epochs,
                        uncertain_event_lower_bound = report.uncertain_event_lower_bound,
                        "unclean request metadata gateway epochs recorded as completeness gaps"
                    ),
                    Ok(report) if report.candidate_epochs > 0 => warn!(
                        candidate_epochs = report.candidate_epochs,
                        "request metadata gateway epochs missed the stale threshold; awaiting confirmation"
                    ),
                    Ok(_) => {}
                    Err(error) => warn!(%error, "request metadata gateway epoch detection failed; retrying"),
                }
            }
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    return;
                }
            }
        }
    }
}

pub(super) async fn outbox_supervisor(
    store: PgStore,
    valkey_url: String,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut backoff = Duration::from_millis(100);
    loop {
        if *shutdown.borrow() {
            return;
        }
        match outbox_loop(store.clone(), &valkey_url, shutdown.clone()).await {
            Ok(()) => return,
            Err(error) => error!(%error, "outbox worker failed; restarting"),
        }
        tokio::select! {
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    return;
                }
            }
            () = tokio::time::sleep(backoff) => {}
        }
        backoff = (backoff * 2).min(Duration::from_secs(5));
    }
}

pub(super) async fn request_metadata_consumer_supervisor(
    store: PgStore,
    valkey_url: String,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut backoff = Duration::from_millis(100);
    loop {
        if *shutdown.borrow() {
            return;
        }
        match request_metadata_consumer_loop(store.clone(), &valkey_url, shutdown.clone()).await {
            Ok(()) => return,
            Err(error) => error!(%error, "request metadata persistence worker failed; restarting"),
        }
        tokio::select! {
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    return;
                }
            }
            () = tokio::time::sleep(backoff) => {}
        }
        backoff = (backoff * 2).min(Duration::from_secs(5));
    }
}

async fn outbox_loop(
    store: PgStore,
    valkey_url: &str,
    mut shutdown: watch::Receiver<bool>,
) -> AppResult<()> {
    let mut publisher = RuntimeHintPublisher::connect(valkey_url).await?;
    let mut interval = tokio::time::interval(Duration::from_millis(250));
    interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! {
            _ = interval.tick() => {
                for record in store.pending_outbox(100).await? {
                    let subscribers = publisher.publish(&record.payload).await?;
                    store.mark_outbox_published(record.id).await?;
                    info!(outbox_id = %record.id, %subscribers, "published runtime hint");
                }
            }
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    return Ok(());
                }
            }
        }
    }
}

async fn request_metadata_consumer_loop(
    store: PgStore,
    valkey_url: &str,
    shutdown: watch::Receiver<bool>,
) -> AppResult<()> {
    run_request_metadata_consumer(&store, valkey_url, shutdown).await?;
    Ok(())
}

pub(super) async fn master_key_command(args: MasterKeyArgs) -> AppResult<()> {
    check_secret_permissions(&args.master_key_file).await?;
    let master_key = load_master_key(&args.master_key_file).await?;
    let store = connect_store(&args.database).await?;
    match args.action {
        MasterKeyAction::Status { batch_size } => {
            let status = store
                .master_key_encryption_status(master_key.version())
                .await?;
            report_master_key_status(&master_key, &status);
            ensure_keyring_covers_references(&master_key, &status)?;
            let verified = store
                .verify_master_key_envelopes(&master_key, batch_size)
                .await?;
            info!(
                active_version = master_key.version(),
                rows_verified = verified.rows_verified,
                "master-key envelope status verified"
            );
        }
        MasterKeyAction::Reencrypt {
            batch_size,
            dry_run,
        } => {
            let initial = store
                .master_key_encryption_status(master_key.version())
                .await?;
            report_master_key_status(&master_key, &initial);
            ensure_keyring_covers_references(&master_key, &initial)?;
            if dry_run {
                let verified = store
                    .verify_master_key_envelopes(&master_key, batch_size)
                    .await?;
                info!(
                    active_version = master_key.version(),
                    rows_verified = verified.rows_verified,
                    rows_requiring_reencryption = initial.non_active_references(),
                    "master-key re-encryption dry run completed without writes"
                );
                return Ok(());
            }
            let mut total_reencrypted = 0_u64;
            loop {
                let status = store
                    .master_key_encryption_status(master_key.version())
                    .await?;
                ensure_keyring_covers_references(&master_key, &status)?;
                if status.non_active_references() == 0 {
                    break;
                }
                let batch = store
                    .reencrypt_master_key_batch(&master_key, batch_size)
                    .await?;
                if batch.rows_reencrypted == 0 {
                    return Err(std::io::Error::other(
                        "master-key re-encryption made no progress while old references remain",
                    )
                    .into());
                }
                total_reencrypted = total_reencrypted.saturating_add(batch.rows_reencrypted);
                for (table, rows) in batch.by_table {
                    info!(
                        active_version = master_key.version(),
                        encrypted_table = table.as_str(),
                        rows_reencrypted = rows,
                        total_reencrypted,
                        "master-key re-encryption batch committed"
                    );
                }
            }
            let verified = store
                .verify_master_key_envelopes(&master_key, batch_size)
                .await?;
            let final_status = store
                .master_key_encryption_status(master_key.version())
                .await?;
            report_master_key_status(&master_key, &final_status);
            if final_status.non_active_references() != 0 {
                return Err(std::io::Error::other(
                    "non-active master-key references appeared during final verification; confirm every replica uses the new active version and rerun",
                )
                .into());
            }
            info!(
                active_version = master_key.version(),
                rows_reencrypted = total_reencrypted,
                rows_verified = verified.rows_verified,
                "master-key re-encryption completed"
            );
        }
        MasterKeyAction::VerifyRetirement {
            version,
            batch_size,
        } => {
            let status = store
                .master_key_encryption_status(master_key.version())
                .await?;
            report_master_key_status(&master_key, &status);
            ensure_keyring_covers_references(&master_key, &status)?;
            let verified = store
                .verify_master_key_retirement(&master_key, version, batch_size)
                .await?;
            info!(
                active_version = master_key.version(),
                retirement_version = version,
                rows_verified = verified.rows_verified,
                "master-key version has zero references and is safe to remove after all replicas use the active keyring"
            );
        }
    }
    Ok(())
}

fn report_master_key_status(master_key: &MasterKey, status: &MasterKeyEncryptionStatus) {
    let available_versions = master_key.versions().collect::<Vec<_>>();
    info!(
        active_version = master_key.version(),
        available_versions = ?available_versions,
        total_encrypted_rows = status.total_references(),
        non_active_references = status.non_active_references(),
        "master-key reference status"
    );
    for reference in &status.references {
        info!(
            encrypted_table = reference.table.as_str(),
            key_version = reference.key_version,
            row_count = reference.row_count,
            "master-key references"
        );
    }
}

pub(super) async fn doctor(args: DoctorArgs) -> AppResult<()> {
    let mut checks = serde_json::Map::new();
    let store = connect_store(&args.backend.database).await?;
    store.ping().await?;
    checks.insert("postgresql".into(), json!({ "ok": true }));

    let limiter = DistributedLimiter::connect(&args.backend.valkey_url, "olp:v2:doctor").await?;
    limiter.ping().await?;
    checks.insert("valkey".into(), json!({ "ok": true }));
    preflight_request_metadata_stream_upgrade(&args.backend.valkey_url).await?;
    checks.insert(
        "request_metadata_stream_upgrade".into(),
        json!({ "ok": true }),
    );

    load_auth_hmac_key(&args.auth_hmac_key_file).await?;
    load_master_key(&args.master_key_file).await?;
    check_secret_permissions(&args.auth_hmac_key_file).await?;
    check_secret_permissions(&args.master_key_file).await?;
    checks.insert("secret_files".into(), json!({ "ok": true }));

    if let Some(path) = &args.connector_config_file {
        let registry = TransportRegistry::default();
        load_connector_config(path, &registry).await?;
        checks.insert(
            "connector_config".into(),
            json!({ "ok": true, "configured": registry.snapshot().len() }),
        );
    }

    if !args.console_dir.join("index.html").is_file() {
        return Err(std::io::Error::other(format!(
            "console index is missing at {}",
            args.console_dir.join("index.html").display()
        ))
        .into());
    }
    checks.insert("console".into(), json!({ "ok": true }));
    let media_spool_dir = args
        .media_spool_dir
        .as_deref()
        .map_or_else(std::env::temp_dir, Path::to_path_buf);
    let media_spool = create_media_spool(&media_spool_dir, args.media_spool_capacity_bytes)?;
    drop(media_spool);
    checks.insert(
        "media_spool".into(),
        json!({
            "ok": true,
            "capacity_bytes": args.media_spool_capacity_bytes,
        }),
    );
    println!(
        "{}",
        serde_json::to_string_pretty(&json!({ "ok": true, "checks": checks }))?
    );
    Ok(())
}
