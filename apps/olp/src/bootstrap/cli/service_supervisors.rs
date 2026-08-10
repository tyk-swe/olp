use std::time::Duration;

use olp_inference::{circuit::CircuitBreaker, limits::ReloadableLimiter};
use olp_storage::{
    PgStore, circuits::DistributedCircuitBreaker, limits::DistributedLimiter,
    request_metadata::RequestMetadataEmitter,
};
use tokio::sync::watch;
use tracing::{error, info, warn};

use crate::reconcile_media_jobs_once;

async fn wait_or_shutdown(shutdown: &mut watch::Receiver<bool>, delay: Duration) -> bool {
    tokio::select! {
        changed = shutdown.changed() => changed.is_err() || *shutdown.borrow(),
        () = tokio::time::sleep(delay) => false,
    }
}

pub(super) async fn media_reconciliation_supervisor(
    state: crate::GatewayState,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut interval = tokio::time::interval(Duration::from_secs(5));
    interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! {
            _ = interval.tick() => {
                match reconcile_media_jobs_once(&state, 16).await {
                    Ok(report) if report.claimed > 0 => {
                        info!(
                            claimed = report.claimed,
                            completed = report.completed,
                            failed = report.failed,
                            "autonomous media reconciliation pass completed"
                        );
                    }
                    Ok(_) => {}
                    Err(error) => warn!(%error, "autonomous media reconciliation pass failed"),
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

pub(super) async fn request_metadata_loss_reporter(
    store: PgStore,
    emitter: RequestMetadataEmitter,
    gateway_instance: String,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut interval = tokio::time::interval(Duration::from_secs(1));
    interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! {
            _ = interval.tick() => {
                let snapshot = emitter.snapshot();
                if let Err(error) = store.report_request_metadata_buffer_loss(&gateway_instance, &snapshot).await {
                    warn!(%error, %gateway_instance, "request metadata loss checkpoint failed; retrying");
                }
            }
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    // Let the stream writer close its receiver and account for
                    // accepted-but-abandoned entries, then durably checkpoint
                    // the final counters before graceful process exit.
                    tokio::time::sleep(Duration::from_millis(100)).await;
                    let deadline = tokio::time::Instant::now() + Duration::from_secs(4);
                    loop {
                        let snapshot = emitter.snapshot();
                        match store.close_request_metadata_buffer_epoch(&gateway_instance, &snapshot).await {
                            Ok(_) => return,
                            Err(error) if tokio::time::Instant::now() < deadline => {
                                warn!(%error, %gateway_instance, "final request metadata loss checkpoint failed; retrying");
                                tokio::time::sleep(Duration::from_millis(200)).await;
                            }
                            Err(error) => {
                                error!(%error, %gateway_instance, lost = snapshot.lost(), "final request metadata loss checkpoint could not be persisted");
                                return;
                            }
                        }
                    }
                }
            }
        }
    }
}

pub(super) async fn limiter_supervisor(
    reloadable_limiter: ReloadableLimiter,
    valkey_url: String,
    limits_namespace: String,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut backoff = Duration::from_millis(100);
    loop {
        if *shutdown.borrow() {
            return;
        }
        if let Some(limiter) = reloadable_limiter.current() {
            let healthy = matches!(
                tokio::time::timeout(Duration::from_secs(1), limiter.ping()).await,
                Ok(Ok(()))
            );
            if healthy {
                if wait_or_shutdown(&mut shutdown, Duration::from_secs(5)).await {
                    return;
                }
                continue;
            }
            reloadable_limiter.clear();
            warn!("Valkey limiter health check failed; hard limits remain fail-closed");
        }

        match tokio::time::timeout(
            Duration::from_secs(3),
            DistributedLimiter::connect(&valkey_url, &limits_namespace),
        )
        .await
        {
            Ok(Ok(limiter)) => {
                reloadable_limiter.install(limiter);
                backoff = Duration::from_millis(100);
                info!("Valkey limiter connection is available");
                continue;
            }
            Ok(Err(error)) => warn!(%error, "Valkey limiter connection failed"),
            Err(_) => warn!("Valkey limiter connection timed out"),
        }
        if wait_or_shutdown(&mut shutdown, backoff).await {
            return;
        }
        backoff = (backoff * 2).min(Duration::from_secs(5));
    }
}

pub(super) async fn circuit_supervisor(
    circuits: CircuitBreaker,
    valkey_url: String,
    circuits_namespace: String,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut backoff = Duration::from_millis(100);
    let mut was_available = circuits.distributed_available();
    loop {
        if *shutdown.borrow() {
            return;
        }
        if matches!(
            tokio::time::timeout(Duration::from_secs(1), circuits.ping_distributed()).await,
            Ok(Some(true))
        ) {
            was_available = true;
            if wait_or_shutdown(&mut shutdown, Duration::from_secs(5)).await {
                return;
            }
            continue;
        }
        if was_available {
            circuits.mark_distributed_unavailable();
            was_available = false;
        }

        match tokio::time::timeout(
            Duration::from_secs(3),
            DistributedCircuitBreaker::connect(&valkey_url, &circuits_namespace),
        )
        .await
        {
            Ok(Ok(distributed)) => {
                circuits.install_distributed(distributed);
                was_available = true;
                backoff = Duration::from_millis(100);
                info!("Valkey circuit coordination is available");
                continue;
            }
            Ok(Err(error)) => warn!(%error, "Valkey circuit coordination connection failed"),
            Err(_) => warn!("Valkey circuit coordination connection timed out"),
        }
        if wait_or_shutdown(&mut shutdown, backoff).await {
            return;
        }
        backoff = (backoff * 2).min(Duration::from_secs(5));
    }
}
