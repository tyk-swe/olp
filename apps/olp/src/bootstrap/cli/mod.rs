mod config;
mod doctor;
mod health_probe;
mod lifecycle;
mod master_key;
mod migrate;
mod runtime_activation;
mod startup;
pub(crate) mod validation;
mod worker;

use std::time::Duration;

use self::{
    config::{Cli, Command},
    doctor::doctor,
    health_probe::health_probe,
    lifecycle::internal_pre_stop,
    master_key::master_key_command,
    migrate::migrate,
    startup::serve,
    worker::run_worker,
};
use crate::application::mode::ApiMode;
use clap::Parser;

use crate::application::error::{AppError, AppResult};
const BACKGROUND_SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(5);

pub fn run() -> AppResult<()> {
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()?;
    runtime.block_on(execute())
}

async fn execute() -> AppResult<()> {
    let command = Cli::parse().command;
    if !matches!(
        &command,
        Command::All(_) | Command::Gateway(_) | Command::Control(_)
    ) {
        crate::observability::tracing::install_logging()?;
    }
    match command {
        Command::All(args) => serve_http_mode(ApiMode::All, args, true).await,
        Command::Gateway(args) => serve_http_mode(ApiMode::Gateway, args, false).await,
        Command::Control(args) => serve_http_mode(ApiMode::Control, args, false).await,
        Command::Worker(args) => run_worker(args).await,
        Command::Migrate(args) => migrate(args).await,
        Command::Doctor(args) => doctor(args).await,
        Command::MasterKey(args) => master_key_command(args).await,
        Command::HealthProbe => health_probe().await,
        Command::InternalPreStop(args) => internal_pre_stop(args).await,
    }
}

async fn serve_http_mode(
    mode: ApiMode,
    args: config::ServeArgs,
    run_worker_in_process: bool,
) -> AppResult<()> {
    let tracing = crate::observability::tracing::Handle::install(
        crate::observability::tracing::Config {
            endpoint: args.tracing.otlp_traces_endpoint.clone(),
            headers_file: args.tracing.otlp_headers_file.clone(),
            sample_ratio: args.tracing.trace_sample_ratio,
            propagate_upstream: args.tracing.trace_propagate_upstream,
            accept_inbound: args.tracing.trace_accept_inbound,
        },
        mode,
    )
    .await?;
    let result = serve(mode, args, run_worker_in_process, tracing.runtime()).await;
    let shutdown = tracing.shutdown().await;
    result?;
    shutdown
}

#[cfg(test)]
mod tests;
