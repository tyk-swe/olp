use crate::process::cli::AppResult;
use crate::process::cli::BACKGROUND_SHUTDOWN_TIMEOUT;
use crate::process::cli::config::PersistenceArgs;
use crate::process::cli::lifecycle::shutdown_signal;
use crate::process::cli::validation::connect_database;
use crate::process::workers::spawn_worker_supervisors;
use crate::process::workers::stop_worker_tasks;
use crate::usage::worker::request_metadata_consumer_name;
use tokio::sync::watch;
use tokio::task::JoinSet;
pub(crate) async fn run_worker(args: PersistenceArgs) -> AppResult<()> {
    let pool = connect_database(&args.database).await?;
    let keyspace = crate::limits::valkey::valkey_keyspace(&pool).await?;
    test_worker_start_barrier().await?;
    let (sender, receiver) = watch::channel(false);
    let mut workers = JoinSet::new();
    spawn_worker_supervisors(
        &mut workers,
        pool,
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
        tokio::time::sleep(std::time::Duration::from_millis(25)).await;
    }
    Ok(())
}

#[cfg(not(all(feature = "test-util", debug_assertions)))]
async fn test_worker_start_barrier() -> AppResult<()> {
    Ok(())
}
