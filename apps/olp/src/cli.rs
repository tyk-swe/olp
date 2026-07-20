mod commands;
mod config;
mod startup;
mod validation;

use std::{error::Error, time::Duration};

use crate::ApiMode;
use clap::Parser;
use tracing_subscriber::EnvFilter;

use self::{
    commands::{doctor, internal_pre_stop, master_key_command, migrate, run_worker_command},
    config::{Cli, Command},
    startup::serve,
};

pub(crate) use validation::check_secret_permissions;

pub(crate) type AppError = Box<dyn Error + Send + Sync>;
pub(crate) type AppResult<T> = Result<T, AppError>;
const BACKGROUND_SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(5);

pub fn run_cli() -> AppResult<()> {
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()?;
    runtime.block_on(run())
}

async fn run() -> AppResult<()> {
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
        Command::Worker(args) => run_worker_command(args).await,
        Command::Migrate(args) => migrate(args).await,
        Command::Doctor(args) => doctor(args).await,
        Command::MasterKey(args) => master_key_command(args).await,
        Command::InternalPreStop(args) => internal_pre_stop(args).await,
    }
}

#[cfg(test)]
mod tests;
