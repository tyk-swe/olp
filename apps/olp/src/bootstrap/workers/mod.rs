pub(crate) mod costs;
pub(crate) mod maintenance;
pub mod outbox;
pub(crate) mod request_metadata;
pub(crate) mod service_supervisors;
use self::{
    costs::cost_reconciliation_supervisor,
    maintenance::maintenance_supervisor,
    outbox::outbox_supervisor,
    request_metadata::{request_metadata_consumer_supervisor, request_metadata_epoch_supervisor},
};
use olp_db::{store::Store, valkey::Keyspace};
use std::time::Duration;
use tokio::{sync::watch, task::JoinSet};
use tracing::warn;
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
    store: Store,
    valkey_url: String,
    keyspace: Keyspace,
    request_metadata_consumer: String,
    shutdown: watch::Receiver<bool>,
) {
    workers.spawn(outbox_supervisor(
        store.clone(),
        valkey_url.clone(),
        keyspace.runtime_hint_channel(),
        shutdown.clone(),
    ));
    workers.spawn(request_metadata_consumer_supervisor(
        store.clone(),
        valkey_url.clone(),
        keyspace.request_metadata_stream(),
        request_metadata_consumer,
        keyspace.limits_namespace(),
        shutdown.clone(),
    ));
    workers.spawn(cost_reconciliation_supervisor(
        store.clone(),
        valkey_url,
        keyspace.limits_namespace(),
        shutdown.clone(),
    ));
    workers.spawn(maintenance_supervisor(store.clone(), shutdown.clone()));
    workers.spawn(request_metadata_epoch_supervisor(store, shutdown));
}
