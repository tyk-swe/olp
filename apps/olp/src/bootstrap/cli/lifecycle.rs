use std::{future::Future, time::Duration};

use futures::future::select_all;
use tokio::{
    sync::{oneshot, watch},
    task::JoinHandle,
};
use tracing::{error, warn};

use super::{AppError, AppResult, config::InternalPreStopArgs};

pub(super) async fn internal_pre_stop(args: InternalPreStopArgs) -> AppResult<()> {
    tokio::time::sleep(Duration::from_secs(args.seconds)).await;
    Ok(())
}

pub(super) async fn shutdown_reason<Signal>(
    signal: Signal,
    request_metadata_writer_status: Option<&mut oneshot::Receiver<AppResult<()>>>,
) -> Option<AppError>
where
    Signal: Future<Output = ()>,
{
    let Some(request_metadata_writer_status) = request_metadata_writer_status else {
        signal.await;
        return None;
    };
    tokio::select! {
        biased;
        status = request_metadata_writer_status => match status {
            Ok(Err(error)) => Some(error),
            Ok(Ok(())) => Some(std::io::Error::other(
                "request metadata stream writer stopped unexpectedly",
            ).into()),
            Err(error) => Some(std::io::Error::other(format!(
                "request metadata stream writer failed without reporting status: {error}",
            )).into()),
        },
        () = signal => None,
    }
}

pub(super) async fn resolve_request_metadata_writer_error(
    request_metadata_writer_status: Option<oneshot::Receiver<AppResult<()>>>,
    terminal_error: Option<AppError>,
) -> Option<AppError> {
    if terminal_error.is_some() {
        return terminal_error;
    }
    let request_metadata_writer_status = request_metadata_writer_status?;
    match request_metadata_writer_status.await {
        Ok(Ok(())) => None,
        Ok(Err(error)) => Some(error),
        Err(error) => Some(
            std::io::Error::other(format!(
                "request metadata stream writer failed without reporting status: {error}",
            ))
            .into(),
        ),
    }
}

pub(super) async fn coordinate_shutdown<Public, Observability, Signal>(
    public_server: Public,
    observability_server: Observability,
    signal: Signal,
    listener_shutdown: watch::Sender<bool>,
    background_shutdown: watch::Sender<bool>,
) -> (Public::Output, Observability::Output, Signal::Output)
where
    Public: Future,
    Observability: Future,
    Signal: Future,
{
    let stop_listeners = async move {
        let output = signal.await;
        let _ = listener_shutdown.send(true);
        output
    };
    let (public_result, observability_result, signal_output) =
        tokio::join!(public_server, observability_server, stop_listeners);
    let _ = background_shutdown.send(true);
    (public_result, observability_result, signal_output)
}

#[cfg(test)]
pub(super) async fn wait_for_shutdown(mut shutdown: watch::Receiver<bool>) {
    loop {
        if *shutdown.borrow() {
            return;
        }
        if shutdown.changed().await.is_err() {
            return;
        }
    }
}

pub(super) async fn stop_background_tasks(mut tasks: Vec<JoinHandle<()>>, timeout: Duration) {
    let deadline = tokio::time::Instant::now() + timeout;
    while !tasks.is_empty() {
        match tokio::time::timeout_at(deadline, select_all(tasks.iter_mut())).await {
            Ok((Ok(()), index, _)) => {
                tasks.swap_remove(index);
            }
            Ok((Err(error), index, _)) => {
                warn!(%error, "background task stopped unexpectedly");
                tasks.swap_remove(index);
            }
            Err(_) => {
                warn!(
                    remaining = tasks.len(),
                    "background tasks did not stop before deadline; aborting them"
                );
                for task in &tasks {
                    task.abort();
                }
                for task in tasks {
                    let _ = task.await;
                }
                break;
            }
        }
    }
}

pub(super) async fn shutdown_signal() {
    let ctrl_c = async {
        if let Err(error) = tokio::signal::ctrl_c().await {
            error!(%error, "Ctrl+C handler is unavailable");
        }
    };
    #[cfg(unix)]
    let terminate = async {
        match tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()) {
            Ok(mut signal) => {
                let _ = signal.recv().await;
            }
            Err(error) => {
                error!(%error, "SIGTERM handler is unavailable");
            }
        }
    };
    #[cfg(not(unix))]
    let terminate = std::future::pending::<()>();
    tokio::select! {
        () = ctrl_c => {},
        () = terminate => {},
    }
}
