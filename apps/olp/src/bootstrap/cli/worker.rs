use super::{
    AppResult, BACKGROUND_SHUTDOWN_TIMEOUT, config::PersistenceArgs, lifecycle::shutdown_signal,
    validation::connect_store,
};
use crate::bootstrap::workers::{
    request_metadata::request_metadata_consumer_name, spawn_worker_supervisors, stop_worker_tasks,
};
use std::time::Duration;
use tokio::{sync::watch, task::JoinSet};
pub(super) async fn run_worker(args: PersistenceArgs) -> AppResult<()> {
    let store = connect_store(&args.database).await?;
    let keyspace = store.valkey_keyspace().await?;
    test_worker_start_barrier().await?;
    let (sender, receiver) = watch::channel(false);
    let mut workers = JoinSet::new();
    spawn_worker_supervisors(
        &mut workers,
        store,
        args.valkey_url,
        keyspace,
        request_metadata_consumer_name(),
        receiver,
    );
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

#[cfg(all(feature = "test-util", debug_assertions))]
async fn test_worker_start_barrier() -> AppResult<()> {
    let Ok(marker) = std::env::var("OLP_TEST_WORKER_START_MARKER") else {
        return Ok(());
    };
    let release = format!("{marker}.release");
    std::fs::write(&marker, b"ready\n")?;
    while !std::path::Path::new(&release).exists() {
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    Ok(())
}

#[cfg(not(all(feature = "test-util", debug_assertions)))]
async fn test_worker_start_barrier() -> AppResult<()> {
    Ok(())
}
