use crate::limits::valkey::Keyspace;
use crate::limits::worker::cost_reconciliation_supervisor;
use crate::runtime::worker::outbox_supervisor;
use crate::usage::retention_worker::maintenance_supervisor;
use crate::usage::worker::request_metadata_consumer_supervisor;
use crate::usage::worker::request_metadata_epoch_supervisor;
use sqlx::PgPool;
use std::time::Duration;
use tokio::sync::watch;
use tokio::task::JoinSet;
use tracing::warn;

pub(crate) const RESTART_BACKOFF_FLOOR: Duration = Duration::from_millis(100);
const RESTART_BACKOFF_CEILING: Duration = Duration::from_secs(5);

/// Returns false once shutdown is requested or the sender is gone.
pub(crate) async fn sleep_unless_shutdown(
    shutdown: &mut watch::Receiver<bool>,
    delay: Duration,
) -> bool {
    tokio::select! {
        changed = shutdown.changed() => !(changed.is_err() || *shutdown.borrow()),
        () = tokio::time::sleep(delay) => true,
    }
}

pub(crate) fn next_restart_backoff(backoff: Duration) -> Duration {
    (backoff * 2).min(RESTART_BACKOFF_CEILING)
}
pub(crate) async fn stop_worker_tasks(
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

pub(crate) fn spawn_worker_supervisors(
    workers: &mut JoinSet<()>,
    pool: PgPool,
    valkey_url: String,
    keyspace: Keyspace,
    request_metadata_consumer: String,
    shutdown: watch::Receiver<bool>,
) {
    workers.spawn(outbox_supervisor(
        pool.clone(),
        valkey_url.clone(),
        keyspace.runtime_hint_channel(),
        shutdown.clone(),
    ));
    workers.spawn(request_metadata_consumer_supervisor(
        pool.clone(),
        valkey_url.clone(),
        keyspace.request_metadata_stream(),
        request_metadata_consumer,
        keyspace.limits_namespace(),
        shutdown.clone(),
    ));
    workers.spawn(cost_reconciliation_supervisor(
        pool.clone(),
        valkey_url,
        keyspace.limits_namespace(),
        shutdown.clone(),
    ));
    workers.spawn(maintenance_supervisor(pool.clone(), shutdown.clone()));
    workers.spawn(request_metadata_epoch_supervisor(pool, shutdown));
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use tokio::sync::watch;

    use crate::process::workers::RESTART_BACKOFF_FLOOR;
    use crate::process::workers::next_restart_backoff;
    use crate::process::workers::sleep_unless_shutdown;

    #[tokio::test(start_paused = true)]
    async fn sleeping_completes_when_shutdown_stays_quiet() {
        let (_sender, mut shutdown) = watch::channel(false);
        let started = tokio::time::Instant::now();
        assert!(sleep_unless_shutdown(&mut shutdown, Duration::from_secs(2)).await);
        assert_eq!(started.elapsed(), Duration::from_secs(2));
    }

    #[tokio::test(start_paused = true)]
    async fn shutdown_request_interrupts_the_sleep() {
        let (sender, mut shutdown) = watch::channel(false);
        tokio::spawn(async move {
            tokio::time::sleep(Duration::from_millis(500)).await;
            sender.send(true).unwrap();
        });
        let started = tokio::time::Instant::now();
        assert!(!sleep_unless_shutdown(&mut shutdown, Duration::from_secs(2)).await);
        assert_eq!(started.elapsed(), Duration::from_millis(500));
    }

    #[tokio::test(start_paused = true)]
    async fn losing_the_shutdown_sender_counts_as_shutdown() {
        let (sender, mut shutdown) = watch::channel(false);
        drop(sender);
        assert!(!sleep_unless_shutdown(&mut shutdown, Duration::from_secs(2)).await);
    }

    #[tokio::test(start_paused = true)]
    async fn a_false_shutdown_signal_ends_the_sleep_early_but_continues() {
        let (sender, mut shutdown) = watch::channel(false);
        tokio::spawn(async move {
            tokio::time::sleep(Duration::from_millis(500)).await;
            sender.send(false).unwrap();
            tokio::time::sleep(Duration::from_secs(10)).await;
        });
        let started = tokio::time::Instant::now();
        assert!(sleep_unless_shutdown(&mut shutdown, Duration::from_secs(2)).await);
        assert_eq!(started.elapsed(), Duration::from_millis(500));
    }

    #[test]
    fn restart_backoff_doubles_and_saturates_at_five_seconds() {
        let mut backoff = RESTART_BACKOFF_FLOOR;
        let mut observed = Vec::new();
        for _ in 0..7 {
            backoff = next_restart_backoff(backoff);
            observed.push(backoff.as_millis());
        }
        assert_eq!(observed, [200, 400, 800, 1600, 3200, 5000, 5000]);
        assert_eq!(
            next_restart_backoff(Duration::from_secs(4)),
            Duration::from_secs(5)
        );
    }
}
