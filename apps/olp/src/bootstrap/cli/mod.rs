mod config;
mod doctor;
mod health_probe;
mod lifecycle;
mod master_key;
mod migrate;
mod runtime_activation;
mod service_supervisors;
mod startup;
pub(crate) mod validation;
#[cfg(feature = "test-util")]
pub mod worker;
#[cfg(not(feature = "test-util"))]
pub(crate) mod worker;

use std::{error::Error, time::Duration};

use crate::bootstrap::state::ApiMode;
use clap::Parser;
use tracing_subscriber::EnvFilter;

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

pub(crate) type AppError = Box<dyn Error + Send + Sync>;
pub(crate) type AppResult<T> = Result<T, AppError>;
const BACKGROUND_SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(5);

pub fn run() -> AppResult<()> {
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()?;
    runtime.block_on(execute())
}

async fn execute() -> AppResult<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("olp=info")),
        )
        .json()
        .init();

    match Cli::parse().command {
        Command::All(args) => serve(ApiMode::All, args, true).await,
        Command::Gateway(args) => serve(ApiMode::Gateway, args, false).await,
        Command::Control(args) => serve(ApiMode::Control, args, false).await,
        Command::Worker(args) => run_worker(args).await,
        Command::Migrate(args) => migrate(args).await,
        Command::Doctor(args) => doctor(args).await,
        Command::MasterKey(args) => master_key_command(args).await,
        Command::HealthProbe => health_probe().await,
        Command::InternalPreStop(args) => internal_pre_stop(args).await,
    }
}

#[cfg(test)]
mod tests;
