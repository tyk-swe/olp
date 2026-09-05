use crate::application::error::AppResult;
use olp_db::valkey::request_metadata::run_request_metadata_consumer;
use olp_db::{
    store::Store,
    worker_health::{WorkerTask, WorkerTaskCheckpointOutcome},
};
use std::time::Duration;
use tokio::sync::watch;
use tracing::{error, warn};
pub(crate) async fn request_metadata_epoch_supervisor(
    store: Store,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut interval = tokio::time::interval(Duration::from_secs(5));
    interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! {
            _ = interval.tick() => {
                match store.detect_stale_request_metadata_gateway_epochs(chrono::Utc::now()).await {
                    Ok(report) => {
                        if let Err(error) = store
                            .report_worker_task_checkpoint(
                                WorkerTask::RequestMetadataGatewayEpochDetection,
                                WorkerTaskCheckpointOutcome::Success,
                                report.candidate_epochs > 0 || report.detected_epochs > 0,
                            )
                            .await
                        {
                            warn!(%error, "request metadata gateway epoch health checkpoint failed");
                        }
                        if report.detected_epochs > 0 {
                            warn!(
                                detected_epochs = report.detected_epochs,
                                uncertain_event_lower_bound = report.uncertain_event_lower_bound,
                                "unclean request metadata gateway epochs recorded as completeness gaps"
                            );
                        } else if report.candidate_epochs > 0 {
                            warn!(
                                candidate_epochs = report.candidate_epochs,
                                "request metadata gateway epochs missed the stale threshold; awaiting confirmation"
                            );
                        }
                    }
                    Err(error) => {
                        if let Err(checkpoint_error) = store
                            .report_worker_task_checkpoint(
                                WorkerTask::RequestMetadataGatewayEpochDetection,
                                WorkerTaskCheckpointOutcome::Failure,
                                false,
                            )
                            .await
                        {
                            warn!(%checkpoint_error, "request metadata gateway epoch failure checkpoint failed");
                        }
                        warn!(%error, "request metadata gateway epoch detection failed; retrying");
                    }
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

pub(crate) async fn request_metadata_consumer_supervisor(
    store: Store,
    valkey_url: String,
    request_metadata_stream: String,
    consumer: String,
    limits_namespace: String,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut backoff = Duration::from_millis(100);
    loop {
        if *shutdown.borrow() {
            return;
        }
        match request_metadata_consumer_loop(
            store.clone(),
            &valkey_url,
            &request_metadata_stream,
            &consumer,
            &limits_namespace,
            shutdown.clone(),
        )
        .await
        {
            Ok(()) => return,
            Err(error) => {
                if let Err(checkpoint_error) = store
                    .report_worker_task_checkpoint(
                        WorkerTask::RequestMetadataConsumer,
                        WorkerTaskCheckpointOutcome::Failure,
                        false,
                    )
                    .await
                {
                    warn!(%checkpoint_error, "request metadata consumer failure checkpoint failed");
                }
                error!(%error, "request metadata persistence worker failed; restarting");
            }
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

async fn request_metadata_consumer_loop(
    store: Store,
    valkey_url: &str,
    request_metadata_stream: &str,
    consumer: &str,
    limits_namespace: &str,
    shutdown: watch::Receiver<bool>,
) -> AppResult<()> {
    run_request_metadata_consumer(
        &store,
        valkey_url,
        request_metadata_stream,
        consumer,
        limits_namespace,
        shutdown,
    )
    .await?;
    Ok(())
}

/// Combines a bounded, log-safe host label with the OS process ID and a UUIDv7
/// process epoch. The supervisor retains this name across transient reconnects.
pub(crate) fn request_metadata_consumer_name() -> String {
    let host = std::env::var("HOSTNAME").unwrap_or_else(|_| "olp".to_owned());
    request_metadata_consumer_name_from(&host, std::process::id(), uuid::Uuid::now_v7())
}

pub(crate) fn request_metadata_consumer_name_from(
    host: &str,
    process_id: u32,
    epoch: uuid::Uuid,
) -> String {
    let mut host = host
        .chars()
        .map(|character| {
            if character.is_ascii_alphanumeric() || character == '-' || character == '_' {
                character.to_ascii_lowercase()
            } else {
                '-'
            }
        })
        .take(48)
        .collect::<String>();
    if host.is_empty() {
        host.push_str("olp");
    }
    format!("{host}-{process_id}-{}", epoch.simple())
}
