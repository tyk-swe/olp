use std::{
    error::Error,
    future::Future,
    net::SocketAddr,
    path::{Path, PathBuf},
    sync::Arc,
    time::Duration,
};

use crate::{
    ApiMode, ApiState, LimiterManager, RuntimeManager, TransportRegistry, TrustedProxyCidr,
    create_media_spool, reconcile_media_jobs_once,
};
use clap::{Args, Parser, Subcommand};
use futures::future::select_all;
use olp_storage::{
    DistributedLimiter, KeyHasher, MasterKey, MasterKeyEncryptionStatus, PgStore,
    RuntimeHintPublisher, RuntimeHintSubscriber, UsageEmitter, run_usage_consumer,
};
use serde_json::json;
use tokio::{
    net::TcpListener,
    sync::watch,
    task::{JoinHandle, JoinSet},
};
use tracing::{error, info, warn};
use tracing_subscriber::EnvFilter;
use zeroize::Zeroizing;

use crate::{
    connectors::{load_connector_config, reload_persisted_connectors},
    listener,
};

pub(crate) type AppError = Box<dyn Error + Send + Sync>;
pub(crate) type AppResult<T> = Result<T, AppError>;
const BACKGROUND_SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(5);

#[derive(Debug, Parser)]
#[command(name = "olp", version, about = "OpenLLMProxy")]
struct Cli {
    #[command(subcommand)]
    command: Command,
}

#[derive(Debug, Subcommand)]
enum Command {
    /// Run gateway, control plane, and background outbox worker together.
    All(ServerArgs),
    /// Run only inference, probes, and metrics.
    Gateway(ServerArgs),
    /// Run only management API, probes, metrics, and static console.
    Control(ServerArgs),
    /// Publish outbox hints and perform asynchronous persistence work.
    Worker(BackendArgs),
    /// Apply PostgreSQL migrations and exit.
    Migrate(MigrateArgs),
    /// Validate dependencies and mounted secrets, then exit.
    Doctor(DoctorArgs),
    /// Inspect, re-encrypt, and verify retirement of master-key versions.
    MasterKey(MasterKeyArgs),
    /// Internal shell-free Kubernetes pre-stop delay.
    #[command(hide = true)]
    InternalPreStop(InternalPreStopArgs),
}

#[derive(Clone, Debug, Args)]
struct InternalPreStopArgs {
    #[arg(long, default_value_t = 10)]
    seconds: u64,
}

#[derive(Clone, Debug, Args)]
struct DatabaseArgs {
    #[arg(long, env = "OLP_DATABASE_URL")]
    database_url: String,
    #[arg(long, env = "OLP_DATABASE_MAX_CONNECTIONS", default_value_t = 20)]
    database_max_connections: u32,
}

#[derive(Clone, Debug, Args)]
struct BackendArgs {
    #[command(flatten)]
    database: DatabaseArgs,
    #[arg(long, env = "OLP_VALKEY_URL")]
    valkey_url: String,
}

#[derive(Clone, Debug, Args)]
struct MigrateArgs {
    #[command(flatten)]
    database: DatabaseArgs,
    /// Test-only target used to construct an N-1 upgrade fixture.
    #[arg(long, hide = true)]
    through_version: Option<i64>,
}

#[derive(Clone, Debug, Args)]
struct ServerArgs {
    #[command(flatten)]
    database: DatabaseArgs,
    #[arg(long, env = "OLP_VALKEY_URL")]
    valkey_url: Option<String>,
    #[arg(long, env = "OLP_LISTEN_ADDR", default_value = "127.0.0.1:8080")]
    listen_addr: SocketAddr,
    /// Private listener for probes and Prometheus metrics. Keep the default
    /// loopback-only unless an internal network is intentionally configured.
    #[arg(
        long,
        env = "OLP_OBSERVABILITY_LISTEN_ADDR",
        default_value = "127.0.0.1:9090"
    )]
    observability_listen_addr: SocketAddr,
    /// Maximum simultaneously admitted TCP connections per HTTP listener.
    #[arg(long, env = "OLP_HTTP_MAX_CONNECTIONS", default_value_t = 1024)]
    http_max_connections: usize,
    /// Cadence for automatic authoritative upstream model inventory refreshes.
    #[arg(
        long,
        env = "OLP_PROVIDER_MODEL_DISCOVERY_INTERVAL_SECONDS",
        default_value_t = 86_400_u64
    )]
    provider_model_discovery_interval_seconds: u64,
    #[arg(
        long,
        env = "OLP_PUBLIC_ORIGIN",
        default_value = "http://127.0.0.1:8080"
    )]
    public_origin: String,
    #[arg(long, env = "OLP_CONSOLE_DIR", default_value = "console/build")]
    console_dir: PathBuf,
    #[arg(long, env = "OLP_MEDIA_SPOOL_DIR")]
    media_spool_dir: Option<PathBuf>,
    #[arg(
        long,
        env = "OLP_MEDIA_SPOOL_CAPACITY_BYTES",
        default_value_t = 1_073_741_824_u64
    )]
    media_spool_capacity_bytes: u64,
    #[arg(long, env = "OLP_KEY_HASH_KEY_FILE")]
    key_hash_key_file: Option<PathBuf>,
    /// Base64-encoded one-time setup token, mounted only in control-plane pods.
    #[arg(long, env = "OLP_BOOTSTRAP_TOKEN_FILE")]
    bootstrap_token_file: Option<PathBuf>,
    /// Comma-separated CIDRs for reverse proxies allowed to supply
    /// X-Forwarded-For for unauthenticated authentication admission.
    #[arg(long, env = "OLP_TRUSTED_PROXY_CIDRS", value_delimiter = ',')]
    trusted_proxy_cidrs: Vec<TrustedProxyCidr>,
    #[arg(long, env = "OLP_MASTER_KEY_FILE")]
    master_key_file: Option<PathBuf>,
    /// JSON file mapping runtime provider IDs to credential files. The JSON
    /// contains paths, never credential values.
    #[arg(long, env = "OLP_CONNECTOR_CONFIG_FILE")]
    connector_config_file: Option<PathBuf>,
}

#[derive(Clone, Debug, Args)]
struct DoctorArgs {
    #[command(flatten)]
    backend: BackendArgs,
    #[arg(long, env = "OLP_CONSOLE_DIR", default_value = "console/build")]
    console_dir: PathBuf,
    #[arg(long, env = "OLP_MEDIA_SPOOL_DIR")]
    media_spool_dir: Option<PathBuf>,
    #[arg(
        long,
        env = "OLP_MEDIA_SPOOL_CAPACITY_BYTES",
        default_value_t = 1_073_741_824_u64
    )]
    media_spool_capacity_bytes: u64,
    #[arg(long, env = "OLP_MASTER_KEY_FILE")]
    master_key_file: PathBuf,
    #[arg(long, env = "OLP_KEY_HASH_KEY_FILE")]
    key_hash_key_file: PathBuf,
    #[arg(long, env = "OLP_CONNECTOR_CONFIG_FILE")]
    connector_config_file: Option<PathBuf>,
}

#[derive(Clone, Debug, Args)]
struct MasterKeyArgs {
    #[command(flatten)]
    database: DatabaseArgs,
    #[arg(long, env = "OLP_MASTER_KEY_FILE")]
    master_key_file: PathBuf,
    #[command(subcommand)]
    action: MasterKeyAction,
}

#[derive(Clone, Debug, Subcommand)]
enum MasterKeyAction {
    /// Count and authenticate every encrypted envelope without changing rows.
    Status {
        #[arg(long, default_value_t = 100)]
        batch_size: u16,
    },
    /// Re-encrypt non-active envelopes in resumable transactional batches.
    Reencrypt {
        #[arg(long, default_value_t = 100)]
        batch_size: u16,
        /// Authenticate all rows and report work without updating ciphertext.
        #[arg(long)]
        dry_run: bool,
    },
    /// Fail unless a decrypt-only version has zero remaining references.
    VerifyRetirement {
        #[arg(long)]
        version: u32,
        #[arg(long, default_value_t = 100)]
        batch_size: u16,
    },
}

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

async fn internal_pre_stop(args: InternalPreStopArgs) -> AppResult<()> {
    tokio::time::sleep(Duration::from_secs(args.seconds)).await;
    Ok(())
}

async fn migrate(args: MigrateArgs) -> AppResult<()> {
    let store = connect_store(&args.database).await?;
    if let Some(target) = args.through_version {
        if std::env::var("OLP_ALLOW_PARTIAL_MIGRATIONS_FOR_TESTS").as_deref() != Ok("test-only") {
            return Err(std::io::Error::other(
                "partial migration targets are restricted to test fixtures",
            )
            .into());
        }
        olp_storage::MIGRATOR.run_to(target, store.pool()).await?;
        info!(target, "PostgreSQL migrations reached test target");
    } else {
        store.migrate().await?;
        info!("PostgreSQL migrations are current");
    }
    Ok(())
}

async fn serve(mode: ApiMode, args: ServerArgs, run_worker_in_process: bool) -> AppResult<()> {
    if args.http_max_connections == 0 {
        return Err(
            std::io::Error::other("OLP_HTTP_MAX_CONNECTIONS must be greater than zero").into(),
        );
    }
    if args.provider_model_discovery_interval_seconds == 0 {
        return Err(std::io::Error::other(
            "OLP_PROVIDER_MODEL_DISCOVERY_INTERVAL_SECONDS must be greater than zero",
        )
        .into());
    }
    if args.provider_model_discovery_interval_seconds > i64::MAX as u64 {
        return Err(std::io::Error::other(
            "OLP_PROVIDER_MODEL_DISCOVERY_INTERVAL_SECONDS is too large",
        )
        .into());
    }
    if mode.serves_control() && args.key_hash_key_file.is_none() {
        return Err(std::io::Error::other(
            "OLP_KEY_HASH_KEY_FILE is required when serving the control plane",
        )
        .into());
    }
    let store = connect_store(&args.database).await?;
    let runtime = Arc::new(RuntimeManager::empty());
    let media_spool_dir = args
        .media_spool_dir
        .clone()
        .unwrap_or_else(std::env::temp_dir);
    let media_spool = create_media_spool(&media_spool_dir, args.media_spool_capacity_bytes)?;
    let mut state = ApiState::new_with_media_spool(
        mode,
        Some(store.clone()),
        runtime,
        args.public_origin,
        args.console_dir,
        media_spool,
    );
    // The browser integration fixture uses a loopback mock identity
    // provider. This branch is compiled out of release binaries, so no
    // deployment setting can weaken the production HTTPS/SSRF policy.
    #[cfg(debug_assertions)]
    if std::env::var("OLP_ALLOW_INSECURE_OIDC_FOR_TESTS").as_deref() == Ok("test-only") {
        state.oidc_allow_insecure_test_endpoints = true;
        warn!("test-only loopback OIDC endpoints are enabled");
    }
    if let Some(path) = &args.key_hash_key_file {
        check_secret_permissions(path).await?;
        state.key_hasher = Some(Arc::new(load_key_hasher(path).await?));
    }
    state.set_trusted_proxy_cidrs(args.trusted_proxy_cidrs.clone());
    let setup_required = if mode.serves_control() {
        store.setup_required().await?
    } else {
        false
    };
    let bootstrap_token_digest = if let Some(path) = &args.bootstrap_token_file {
        check_secret_permissions(path).await?;
        let hasher = state.key_hasher.as_deref().ok_or_else(|| {
            std::io::Error::other(
                "OLP_BOOTSTRAP_TOKEN_FILE requires OLP_KEY_HASH_KEY_FILE for digest verification",
            )
        })?;
        Some(load_bootstrap_token_digest(path, hasher).await?)
    } else {
        None
    };
    if setup_required {
        let digest = bootstrap_token_digest.ok_or_else(|| {
            std::io::Error::other(
                "database setup is incomplete; OLP_BOOTSTRAP_TOKEN_FILE is required before serving the control plane",
            )
        })?;
        state.set_bootstrap_token_digest(digest);
    }
    if let Some(path) = &args.master_key_file {
        check_secret_permissions(path).await?;
        state.master_key = Some(Arc::new(load_master_key(path).await?));
    }
    if let Some(path) = &args.connector_config_file {
        load_connector_config(path, &state.transports).await?;
    }
    match activate_latest_runtime(
        &state.runtime,
        &store,
        &state.transports,
        state.master_key.as_deref(),
    )
    .await
    {
        Ok(true) => info!(generation = ?state.runtime.ordinal(), "loaded runtime generation"),
        Ok(false) => warn!("no active runtime generation; gateway will remain unready"),
        Err(error) => error!(%error, "initial runtime release was rejected"),
    }
    let listener = TcpListener::bind(args.listen_addr).await?;
    let observability_listener = TcpListener::bind(args.observability_listen_addr).await?;
    let (background_shutdown_sender, background_shutdown_receiver) = watch::channel(false);
    let (listener_shutdown_sender, listener_shutdown_receiver) = watch::channel(false);
    let mut background_tasks: Vec<JoinHandle<()>> = Vec::new();
    background_tasks.push(spawn_runtime_poller(
        Arc::clone(&state.runtime),
        store.clone(),
        state.transports.clone(),
        state.master_key.clone(),
        background_shutdown_receiver.clone(),
    ));
    if mode.serves_gateway() {
        background_tasks.push(tokio::spawn(media_reconciliation_supervisor(
            state.clone(),
            background_shutdown_receiver.clone(),
        )));
    }
    if mode.serves_control() {
        if state.master_key.is_some() {
            background_tasks.push(tokio::spawn(model_discovery_supervisor(
                state.clone(),
                Duration::from_secs(args.provider_model_discovery_interval_seconds),
                background_shutdown_receiver.clone(),
            )));
        } else {
            warn!("automatic model discovery is disabled because no master key is configured");
        }
    }
    if let Some(url) = &args.valkey_url {
        background_tasks.push(tokio::spawn(runtime_hint_supervisor(
            Arc::clone(&state.runtime),
            store.clone(),
            state.transports.clone(),
            state.master_key.clone(),
            url.clone(),
            background_shutdown_receiver.clone(),
        )));
        state.limiter.mark_configured();
        background_tasks.push(tokio::spawn(limiter_supervisor(
            state.limiter.clone(),
            url.clone(),
            background_shutdown_receiver.clone(),
        )));

        if mode.serves_gateway() {
            // Install the bounded local emitter even when Valkey is not up yet.
            // Its connection loop exposes retry/pending state and preserves events
            // until the configured bound is reached.
            let (emitter, receiver) = UsageEmitter::bounded(8_192);
            state.usage = Some(emitter.clone());
            let gateway_instance = format!(
                "{}:{}",
                std::env::var("HOSTNAME").unwrap_or_else(|_| "olp".to_owned()),
                args.listen_addr
            );
            background_tasks.push(tokio::spawn(usage_loss_reporter(
                store.clone(),
                emitter,
                gateway_instance,
                background_shutdown_receiver.clone(),
            )));
            let usage_writer_url = url.clone();
            let usage_writer_shutdown = background_shutdown_receiver.clone();
            background_tasks.push(tokio::spawn(async move {
                if let Err(error) = receiver
                    .run_connecting(&usage_writer_url, "olp:v2:usage", usage_writer_shutdown)
                    .await
                {
                    error!(%error, "usage stream writer stopped");
                }
            }));
        }
        if run_worker_in_process {
            background_tasks.push(tokio::spawn(outbox_supervisor(
                store.clone(),
                url.clone(),
                background_shutdown_receiver.clone(),
            )));
            background_tasks.push(tokio::spawn(usage_consumer_supervisor(
                store.clone(),
                url.clone(),
                background_shutdown_receiver.clone(),
            )));
        }
    }
    if run_worker_in_process {
        background_tasks.push(tokio::spawn(maintenance_supervisor(
            store.clone(),
            background_shutdown_receiver.clone(),
        )));
        background_tasks.push(tokio::spawn(usage_epoch_supervisor(
            store.clone(),
            background_shutdown_receiver.clone(),
        )));
    }
    background_tasks.push(crate::spawn_observability_cache(
        state.clone(),
        background_shutdown_receiver.clone(),
    ));

    info!(address = %args.listen_addr, ?mode, "OLP public listener ready");
    info!(address = %args.observability_listen_addr, ?mode, "OLP observability listener ready");
    let public_server = listener::serve_http(
        listener,
        crate::try_public_router(state.clone())?,
        listener::HttpServerConfig::public(args.http_max_connections),
        listener_shutdown_receiver.clone(),
    );
    // This listener has its own router-level concurrency cap. Constrain its
    // connection envelope too so metrics traffic cannot occupy the public
    // listener's entire process-level resource budget.
    let observability_server = listener::serve_http(
        observability_listener,
        crate::observability_router(state),
        listener::HttpServerConfig::public(args.http_max_connections.clamp(1, 32)),
        listener_shutdown_receiver,
    );
    let (public_result, observability_result) = coordinate_shutdown(
        public_server,
        observability_server,
        shutdown_signal(),
        listener_shutdown_sender,
        background_shutdown_sender,
    )
    .await;
    stop_background_tasks(background_tasks, BACKGROUND_SHUTDOWN_TIMEOUT).await;
    public_result?;
    observability_result?;
    Ok(())
}

async fn coordinate_shutdown<Public, Observability, Signal>(
    public_server: Public,
    observability_server: Observability,
    signal: Signal,
    listener_shutdown: watch::Sender<bool>,
    background_shutdown: watch::Sender<bool>,
) -> (Public::Output, Observability::Output)
where
    Public: Future,
    Observability: Future,
    Signal: Future<Output = ()>,
{
    let stop_listeners = async move {
        signal.await;
        let _ = listener_shutdown.send(true);
    };
    let (public_result, observability_result, ()) =
        tokio::join!(public_server, observability_server, stop_listeners);
    let _ = background_shutdown.send(true);
    (public_result, observability_result)
}

#[cfg(test)]
async fn wait_for_shutdown(mut shutdown: watch::Receiver<bool>) {
    loop {
        if *shutdown.borrow() {
            return;
        }
        if shutdown.changed().await.is_err() {
            return;
        }
    }
}

async fn model_discovery_supervisor(
    state: ApiState,
    discovery_interval: Duration,
    mut shutdown: watch::Receiver<bool>,
) {
    // A short local tick only finds rows that are due according to their
    // durable timestamps. The PostgreSQL claim makes this safe across control
    // replicas and avoids waiting a full cadence after a process restart.
    let mut interval = tokio::time::interval(model_discovery_poll_interval(discovery_interval));
    interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    let interval_seconds = i64::try_from(discovery_interval.as_secs())
        .expect("validated model discovery interval fits i64");
    let discovery_interval = chrono::Duration::seconds(interval_seconds);
    loop {
        tokio::select! {
            _ = interval.tick() => {
                match crate::catalog::run_scheduled_model_discovery_once(&state, discovery_interval).await {
                    Ok(completed) if completed > 0 => info!(completed, "scheduled provider model discovery pass completed"),
                    Ok(_) => {}
                    Err(error) => warn!(%error, "scheduled provider model discovery pass failed"),
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

fn model_discovery_poll_interval(discovery_interval: Duration) -> Duration {
    discovery_interval.min(Duration::from_secs(60))
}

async fn media_reconciliation_supervisor(state: ApiState, mut shutdown: watch::Receiver<bool>) {
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

async fn usage_loss_reporter(
    store: PgStore,
    emitter: UsageEmitter,
    gateway_instance: String,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut interval = tokio::time::interval(Duration::from_secs(1));
    interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! {
            _ = interval.tick() => {
                let snapshot = emitter.snapshot();
                if let Err(error) = store.report_usage_buffer_loss(&gateway_instance, &snapshot).await {
                    warn!(%error, %gateway_instance, "usage-loss checkpoint failed; retrying");
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
                        match store.close_usage_buffer_epoch(&gateway_instance, &snapshot).await {
                            Ok(_) => return,
                            Err(error) if tokio::time::Instant::now() < deadline => {
                                warn!(%error, %gateway_instance, "final usage-loss checkpoint failed; retrying");
                                tokio::time::sleep(Duration::from_millis(200)).await;
                            }
                            Err(error) => {
                                error!(%error, %gateway_instance, lost = snapshot.lost(), "final usage-loss checkpoint could not be persisted");
                                return;
                            }
                        }
                    }
                }
            }
        }
    }
}

async fn runtime_hint_supervisor(
    runtime: Arc<RuntimeManager>,
    store: PgStore,
    transports: TransportRegistry,
    master_key: Option<Arc<MasterKey>>,
    valkey_url: String,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut backoff = Duration::from_millis(100);
    loop {
        if *shutdown.borrow() {
            return;
        }
        let result: AppResult<()> = async {
            let mut subscriber = RuntimeHintSubscriber::connect(&valkey_url).await?;
            backoff = Duration::from_millis(100);
            loop {
                tokio::select! {
                    changed = shutdown.changed() => {
                        if changed.is_err() || *shutdown.borrow() {
                            return Ok(());
                        }
                    }
                    hint = subscriber.recv() => {
                        hint?;
                        match activate_latest_runtime(
                            &runtime,
                            &store,
                            &transports,
                            master_key.as_deref(),
                        )
                        .await
                        {
                            Ok(true) => info!(generation = ?runtime.ordinal(), "runtime hint activated generation"),
                            Ok(false) => {}
                            Err(error) => error!(%error, "runtime hint rejected; retaining last-known-good"),
                        }
                    }
                }
            }
        }
        .await;
        if *shutdown.borrow() {
            return;
        }
        if let Err(error) = result {
            warn!(%error, "runtime hint subscriber failed; polling remains active");
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

fn spawn_runtime_poller(
    runtime: Arc<RuntimeManager>,
    store: PgStore,
    transports: TransportRegistry,
    master_key: Option<Arc<MasterKey>>,
    mut shutdown: watch::Receiver<bool>,
) -> JoinHandle<()> {
    tokio::spawn(async move {
        let mut interval = tokio::time::interval(Duration::from_secs(5));
        interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        loop {
            tokio::select! {
                _ = interval.tick() => {
                    match activate_latest_runtime(&runtime, &store, &transports, master_key.as_deref())
                        .await
                    {
                        Ok(true) => {
                            info!(generation = ?runtime.ordinal(), "runtime generation activated");
                        }
                        Ok(false) => {}
                        Err(error) => {
                            // Keep serving the last-known-good Arc. A bad release never
                            // partially changes live indexes.
                            error!(%error, "runtime poll rejected release; retaining last-known-good")
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
    })
}

async fn stop_background_tasks(mut tasks: Vec<JoinHandle<()>>, timeout: Duration) {
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

async fn run_worker_command(args: BackendArgs) -> AppResult<()> {
    let store = connect_store(&args.database).await?;
    let (sender, receiver) = watch::channel(false);
    let mut workers = JoinSet::new();
    spawn_worker_supervisors(&mut workers, store, args.valkey_url, receiver);
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

async fn stop_worker_tasks(
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

fn spawn_worker_supervisors(
    workers: &mut JoinSet<()>,
    store: PgStore,
    valkey_url: String,
    shutdown: watch::Receiver<bool>,
) {
    workers.spawn(outbox_supervisor(
        store.clone(),
        valkey_url.clone(),
        shutdown.clone(),
    ));
    workers.spawn(usage_consumer_supervisor(
        store.clone(),
        valkey_url,
        shutdown.clone(),
    ));
    workers.spawn(maintenance_supervisor(store.clone(), shutdown.clone()));
    workers.spawn(usage_epoch_supervisor(store, shutdown));
}

async fn maintenance_supervisor(store: PgStore, mut shutdown: watch::Receiver<bool>) {
    // Frequent bounded passes keep receipt expiry from becoming one large
    // hourly DELETE/WAL spike at qualified request rates.
    let mut interval = tokio::time::interval(Duration::from_secs(60));
    interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! {
            _ = interval.tick() => {
                match store.run_maintenance(chrono::Utc::now()).await {
                    Ok(report) => info!(?report, "maintenance pass completed"),
                    Err(error) => error!(%error, "maintenance pass failed; retrying next interval"),
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

async fn usage_epoch_supervisor(store: PgStore, mut shutdown: watch::Receiver<bool>) {
    let mut interval = tokio::time::interval(Duration::from_secs(5));
    interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! {
            _ = interval.tick() => {
                match store.detect_stale_usage_gateway_epochs(chrono::Utc::now()).await {
                    Ok(report) if report.detected_epochs > 0 => warn!(
                        detected_epochs = report.detected_epochs,
                        uncertain_event_lower_bound = report.uncertain_event_lower_bound,
                        "unclean gateway usage epochs recorded as completeness gaps"
                    ),
                    Ok(report) if report.candidate_epochs > 0 => warn!(
                        candidate_epochs = report.candidate_epochs,
                        "gateway usage epochs missed the stale threshold; awaiting confirmation"
                    ),
                    Ok(_) => {}
                    Err(error) => warn!(%error, "gateway usage-epoch detection failed; retrying"),
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

async fn limiter_supervisor(
    manager: LimiterManager,
    valkey_url: String,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut backoff = Duration::from_millis(100);
    loop {
        if *shutdown.borrow() {
            return;
        }
        if let Some(limiter) = manager.get() {
            let healthy = matches!(
                tokio::time::timeout(Duration::from_secs(1), limiter.ping()).await,
                Ok(Ok(()))
            );
            if healthy {
                tokio::select! {
                    changed = shutdown.changed() => {
                        if changed.is_err() || *shutdown.borrow() {
                            return;
                        }
                    }
                    () = tokio::time::sleep(Duration::from_secs(5)) => {}
                }
                continue;
            }
            manager.clear();
            warn!("Valkey limiter health check failed; hard limits remain fail-closed");
        }

        match tokio::time::timeout(
            Duration::from_secs(3),
            DistributedLimiter::connect(&valkey_url, "olp:v2:limits"),
        )
        .await
        {
            Ok(Ok(limiter)) => {
                manager.install(limiter);
                backoff = Duration::from_millis(100);
                info!("Valkey limiter connection is available");
            }
            Ok(Err(error)) => warn!(%error, "Valkey limiter connection failed"),
            Err(_) => warn!("Valkey limiter connection timed out"),
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

async fn outbox_supervisor(
    store: PgStore,
    valkey_url: String,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut backoff = Duration::from_millis(100);
    loop {
        if *shutdown.borrow() {
            return;
        }
        match outbox_loop(store.clone(), &valkey_url, shutdown.clone()).await {
            Ok(()) => return,
            Err(error) => error!(%error, "outbox worker failed; restarting"),
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

async fn usage_consumer_supervisor(
    store: PgStore,
    valkey_url: String,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut backoff = Duration::from_millis(100);
    loop {
        if *shutdown.borrow() {
            return;
        }
        match usage_consumer_loop(store.clone(), &valkey_url, shutdown.clone()).await {
            Ok(()) => return,
            Err(error) => error!(%error, "usage persistence worker failed; restarting"),
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

async fn outbox_loop(
    store: PgStore,
    valkey_url: &str,
    mut shutdown: watch::Receiver<bool>,
) -> AppResult<()> {
    let mut publisher = RuntimeHintPublisher::connect(valkey_url).await?;
    let mut interval = tokio::time::interval(Duration::from_millis(250));
    interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! {
            _ = interval.tick() => {
                for record in store.pending_outbox(100).await? {
                    let subscribers = publisher.publish(&record.payload).await?;
                    store.mark_outbox_published(record.id).await?;
                    info!(outbox_id = %record.id, %subscribers, "published runtime hint");
                }
            }
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    return Ok(());
                }
            }
        }
    }
}

async fn usage_consumer_loop(
    store: PgStore,
    valkey_url: &str,
    shutdown: watch::Receiver<bool>,
) -> AppResult<()> {
    run_usage_consumer(&store, valkey_url, shutdown).await?;
    Ok(())
}

async fn master_key_command(args: MasterKeyArgs) -> AppResult<()> {
    check_secret_permissions(&args.master_key_file).await?;
    let master_key = load_master_key(&args.master_key_file).await?;
    let store = connect_store(&args.database).await?;
    match args.action {
        MasterKeyAction::Status { batch_size } => {
            let status = store
                .master_key_encryption_status(master_key.version())
                .await?;
            report_master_key_status(&master_key, &status);
            ensure_keyring_covers_references(&master_key, &status)?;
            let verified = store
                .verify_master_key_envelopes(&master_key, batch_size)
                .await?;
            info!(
                active_version = master_key.version(),
                rows_verified = verified.rows_verified,
                "master-key envelope status verified"
            );
        }
        MasterKeyAction::Reencrypt {
            batch_size,
            dry_run,
        } => {
            let initial = store
                .master_key_encryption_status(master_key.version())
                .await?;
            report_master_key_status(&master_key, &initial);
            ensure_keyring_covers_references(&master_key, &initial)?;
            if dry_run {
                let verified = store
                    .verify_master_key_envelopes(&master_key, batch_size)
                    .await?;
                info!(
                    active_version = master_key.version(),
                    rows_verified = verified.rows_verified,
                    rows_requiring_reencryption = initial.non_active_references(),
                    "master-key re-encryption dry run completed without writes"
                );
                return Ok(());
            }
            let mut total_reencrypted = 0_u64;
            loop {
                let status = store
                    .master_key_encryption_status(master_key.version())
                    .await?;
                ensure_keyring_covers_references(&master_key, &status)?;
                if status.non_active_references() == 0 {
                    break;
                }
                let batch = store
                    .reencrypt_master_key_batch(&master_key, batch_size)
                    .await?;
                if batch.rows_reencrypted == 0 {
                    return Err(std::io::Error::other(
                        "master-key re-encryption made no progress while old references remain",
                    )
                    .into());
                }
                total_reencrypted = total_reencrypted.saturating_add(batch.rows_reencrypted);
                for (table, rows) in batch.by_table {
                    info!(
                        active_version = master_key.version(),
                        encrypted_table = table.as_str(),
                        rows_reencrypted = rows,
                        total_reencrypted,
                        "master-key re-encryption batch committed"
                    );
                }
            }
            let verified = store
                .verify_master_key_envelopes(&master_key, batch_size)
                .await?;
            let final_status = store
                .master_key_encryption_status(master_key.version())
                .await?;
            report_master_key_status(&master_key, &final_status);
            if final_status.non_active_references() != 0 {
                return Err(std::io::Error::other(
                    "non-active master-key references appeared during final verification; confirm every replica uses the new active version and rerun",
                )
                .into());
            }
            info!(
                active_version = master_key.version(),
                rows_reencrypted = total_reencrypted,
                rows_verified = verified.rows_verified,
                "master-key re-encryption completed"
            );
        }
        MasterKeyAction::VerifyRetirement {
            version,
            batch_size,
        } => {
            let status = store
                .master_key_encryption_status(master_key.version())
                .await?;
            report_master_key_status(&master_key, &status);
            ensure_keyring_covers_references(&master_key, &status)?;
            let verified = store
                .verify_master_key_retirement(&master_key, version, batch_size)
                .await?;
            info!(
                active_version = master_key.version(),
                retirement_version = version,
                rows_verified = verified.rows_verified,
                "master-key version has zero references and is safe to remove after all replicas use the active keyring"
            );
        }
    }
    Ok(())
}

fn report_master_key_status(master_key: &MasterKey, status: &MasterKeyEncryptionStatus) {
    let available_versions = master_key.versions().collect::<Vec<_>>();
    info!(
        active_version = master_key.version(),
        available_versions = ?available_versions,
        total_encrypted_rows = status.total_references(),
        non_active_references = status.non_active_references(),
        "master-key reference status"
    );
    for reference in &status.references {
        info!(
            encrypted_table = reference.table.as_str(),
            key_version = reference.key_version,
            row_count = reference.row_count,
            "master-key references"
        );
    }
}

fn ensure_keyring_covers_references(
    master_key: &MasterKey,
    status: &MasterKeyEncryptionStatus,
) -> AppResult<()> {
    if let Some(reference) = status
        .references
        .iter()
        .find(|reference| !master_key.contains_version(reference.key_version))
    {
        return Err(std::io::Error::other(format!(
            "mounted master-key keyring is missing referenced version {}",
            reference.key_version
        ))
        .into());
    }
    Ok(())
}

async fn doctor(args: DoctorArgs) -> AppResult<()> {
    let mut checks = serde_json::Map::new();
    let store = connect_store(&args.backend.database).await?;
    store.ping().await?;
    checks.insert("postgresql".into(), json!({ "ok": true }));

    let limiter = DistributedLimiter::connect(&args.backend.valkey_url, "olp:v2:doctor").await?;
    limiter.ping().await?;
    checks.insert("valkey".into(), json!({ "ok": true }));

    load_key_hasher(&args.key_hash_key_file).await?;
    load_master_key(&args.master_key_file).await?;
    check_secret_permissions(&args.key_hash_key_file).await?;
    check_secret_permissions(&args.master_key_file).await?;
    checks.insert("secret_files".into(), json!({ "ok": true }));

    if let Some(path) = &args.connector_config_file {
        let registry = TransportRegistry::default();
        load_connector_config(path, &registry).await?;
        checks.insert(
            "connector_config".into(),
            json!({ "ok": true, "configured": registry.snapshot().len() }),
        );
    }

    if !args.console_dir.join("index.html").is_file() {
        return Err(std::io::Error::other(format!(
            "console index is missing at {}",
            args.console_dir.join("index.html").display()
        ))
        .into());
    }
    checks.insert("console".into(), json!({ "ok": true }));
    let media_spool_dir = args
        .media_spool_dir
        .as_deref()
        .map_or_else(std::env::temp_dir, Path::to_path_buf);
    let media_spool = create_media_spool(&media_spool_dir, args.media_spool_capacity_bytes)?;
    drop(media_spool);
    checks.insert(
        "media_spool".into(),
        json!({
            "ok": true,
            "capacity_bytes": args.media_spool_capacity_bytes,
        }),
    );
    println!(
        "{}",
        serde_json::to_string_pretty(&json!({ "ok": true, "checks": checks }))?
    );
    Ok(())
}

async fn connect_store(args: &DatabaseArgs) -> AppResult<PgStore> {
    Ok(PgStore::connect(&args.database_url, args.database_max_connections).await?)
}

async fn load_key_hasher(path: &Path) -> AppResult<KeyHasher> {
    let encoded = Zeroizing::new(tokio::fs::read_to_string(path).await?);
    Ok(KeyHasher::from_base64(&encoded)?)
}

async fn load_bootstrap_token_digest(path: &Path, hasher: &KeyHasher) -> AppResult<[u8; 32]> {
    let encoded = Zeroizing::new(tokio::fs::read_to_string(path).await?);
    Ok(hasher.bootstrap_token_digest_from_base64(&encoded)?)
}

async fn load_master_key(path: &Path) -> AppResult<MasterKey> {
    let encoded = Zeroizing::new(tokio::fs::read_to_string(path).await?);
    Ok(MasterKey::from_file_contents(&encoded)?)
}

async fn activate_latest_runtime(
    runtime: &RuntimeManager,
    store: &PgStore,
    transports: &TransportRegistry,
    master_key: Option<&MasterKey>,
) -> AppResult<bool> {
    let releases = store.recent_valid_releases(32).await?;
    if releases.is_empty() {
        return Ok(false);
    }
    let current_api_keys = store.current_runtime_api_keys().await?;
    let mut rejected = Vec::new();
    for release in releases {
        if runtime
            .ordinal()
            .is_some_and(|ordinal| ordinal >= u64::try_from(release.sequence).unwrap_or(u64::MAX))
        {
            continue;
        }
        let snapshot = match runtime.decode_release_candidate(&release, current_api_keys.clone()) {
            Ok(snapshot) => snapshot,
            Err(error) => {
                rejected.push(format!("{}: {error}", release.sequence));
                continue;
            }
        };
        // Provider transports are assembled from normalized secret storage, not
        // the public runtime payload. Require the release-time sidecar to match
        // every current transport-affecting field before accepting an LKG.
        if let Err(error) = store.provider_secrets_for_runtime(&snapshot).await {
            rejected.push(format!("{}: {error}", release.sequence));
            continue;
        }
        let mut candidate_transports = transports.snapshot();
        if let Some(master_key) = master_key
            && let Err(error) =
                reload_persisted_connectors(store, master_key, &snapshot, &mut candidate_transports)
                    .await
        {
            rejected.push(format!("{}: {error}", release.sequence));
            continue;
        }
        candidate_transports.retain(|provider_id, _| snapshot.providers.contains_key(provider_id));
        match runtime.install(snapshot, candidate_transports) {
            Ok(installed) => {
                if !rejected.is_empty() {
                    warn!(
                        rejected = ?rejected,
                        selected_sequence = release.sequence,
                        "installed previous verified runtime release after rejecting newer candidates"
                    );
                }
                return Ok(installed);
            }
            Err(error) => rejected.push(format!("{}: {error}", release.sequence)),
        }
    }
    if rejected.is_empty() {
        return Ok(false);
    }
    Err(std::io::Error::other(format!(
        "no verified runtime release could be installed: {}",
        rejected.join("; ")
    ))
    .into())
}

#[cfg(unix)]
pub(crate) async fn check_secret_permissions(path: &Path) -> AppResult<()> {
    use std::os::unix::fs::PermissionsExt;
    let mode = tokio::fs::metadata(path).await?.permissions().mode() & 0o777;
    if mode & 0o007 != 0 {
        return Err(std::io::Error::other(format!(
            "{} must not be accessible by other users",
            path.display()
        ))
        .into());
    }
    Ok(())
}

#[cfg(not(unix))]
async fn check_secret_permissions(path: &Path) -> AppResult<()> {
    tokio::fs::metadata(path).await?;
    Ok(())
}

async fn shutdown_signal() {
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

#[cfg(test)]
mod tests {
    use std::{
        io::Write as _,
        sync::atomic::{AtomicBool, AtomicUsize, Ordering},
    };

    use olp_storage::{EncryptedTable, KeyVersionReference};
    use tempfile::NamedTempFile;

    use super::*;

    struct DropSignal(Arc<AtomicBool>);

    impl Drop for DropSignal {
        fn drop(&mut self) {
            self.0.store(true, Ordering::Release);
        }
    }

    fn write_temp_file(contents: impl AsRef<[u8]>) -> NamedTempFile {
        let mut file = NamedTempFile::new().unwrap();
        file.write_all(contents.as_ref()).unwrap();
        file
    }

    #[cfg(unix)]
    fn set_file_mode(path: &Path, mode: u32) {
        use std::os::unix::fs::PermissionsExt;

        std::fs::set_permissions(path, std::fs::Permissions::from_mode(mode)).unwrap();
    }

    #[test]
    fn master_key_cli_exposes_status_dry_run_and_retirement_guards() {
        let status = Cli::try_parse_from([
            "olp",
            "master-key",
            "--database-url",
            "postgres://example/olp",
            "--master-key-file",
            "/run/secrets/master-key",
            "status",
            "--batch-size",
            "25",
        ])
        .unwrap();
        assert!(matches!(
            status.command,
            Command::MasterKey(MasterKeyArgs {
                action: MasterKeyAction::Status { batch_size: 25 },
                ..
            })
        ));

        let dry_run = Cli::try_parse_from([
            "olp",
            "master-key",
            "--database-url",
            "postgres://example/olp",
            "--master-key-file",
            "/run/secrets/master-key",
            "reencrypt",
            "--dry-run",
        ])
        .unwrap();
        assert!(matches!(
            dry_run.command,
            Command::MasterKey(MasterKeyArgs {
                action: MasterKeyAction::Reencrypt { dry_run: true, .. },
                ..
            })
        ));

        let retirement = Cli::try_parse_from([
            "olp",
            "master-key",
            "--database-url",
            "postgres://example/olp",
            "--master-key-file",
            "/run/secrets/master-key",
            "verify-retirement",
            "--version",
            "1",
        ])
        .unwrap();
        assert!(matches!(
            retirement.command,
            Command::MasterKey(MasterKeyArgs {
                action: MasterKeyAction::VerifyRetirement { version: 1, .. },
                ..
            })
        ));
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn secret_files_reject_world_access_but_accept_owner_only_permissions() {
        let secret = write_temp_file(b"mounted-secret");

        set_file_mode(secret.path(), 0o600);
        check_secret_permissions(secret.path()).await.unwrap();

        set_file_mode(secret.path(), 0o604);
        let error = check_secret_permissions(secret.path()).await.unwrap_err();
        assert!(
            error
                .to_string()
                .contains("must not be accessible by other users")
        );
    }

    #[tokio::test]
    async fn bootstrap_token_file_is_base64_decoded_to_a_digest() {
        let token = write_temp_file("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n");
        #[cfg(unix)]
        set_file_mode(token.path(), 0o600);
        let hasher = KeyHasher::new([9; 32]);
        let digest = load_bootstrap_token_digest(token.path(), &hasher)
            .await
            .unwrap();
        assert!(hasher.verify_bootstrap_token_digest(
            "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
            &digest
        ));
    }

    #[test]
    fn server_cli_parses_bootstrap_and_trusted_proxy_configuration() {
        let cli = Cli::try_parse_from([
            "olp",
            "control",
            "--database-url",
            "postgres://example/olp",
            "--key-hash-key-file",
            "/run/secrets/key-hash",
            "--bootstrap-token-file",
            "/run/secrets/bootstrap",
            "--trusted-proxy-cidrs",
            "10.0.0.0/8,2001:db8::/32",
        ])
        .unwrap();
        let Command::Control(args) = cli.command else {
            panic!("expected control command");
        };
        assert_eq!(
            args.bootstrap_token_file.unwrap(),
            PathBuf::from("/run/secrets/bootstrap")
        );
        assert_eq!(args.trusted_proxy_cidrs.len(), 2);
    }

    #[test]
    fn model_discovery_polling_honors_sub_minute_cadences() {
        assert_eq!(
            model_discovery_poll_interval(Duration::from_secs(15)),
            Duration::from_secs(15)
        );
        assert_eq!(
            model_discovery_poll_interval(Duration::from_secs(60)),
            Duration::from_secs(60)
        );
        assert_eq!(
            model_discovery_poll_interval(Duration::from_secs(86_400)),
            Duration::from_secs(60)
        );
    }

    #[test]
    fn mounted_master_key_versions_must_cover_every_reference() {
        let master_key = MasterKey::from_file_contents(
            r#"{
                "active_version": 2,
                "keys": [
                    {"version": 1, "key": "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="},
                    {"version": 2, "key": "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI="}
                ]
            }"#,
        )
        .unwrap();
        let covered = MasterKeyEncryptionStatus {
            active_version: 2,
            references: vec![
                KeyVersionReference {
                    table: EncryptedTable::ProviderCredentialVersions,
                    key_version: 1,
                    row_count: 2,
                },
                KeyVersionReference {
                    table: EncryptedTable::OidcConfigurations,
                    key_version: 2,
                    row_count: 1,
                },
            ],
        };
        ensure_keyring_covers_references(&master_key, &covered).unwrap();

        let missing = MasterKeyEncryptionStatus {
            active_version: 2,
            references: vec![KeyVersionReference {
                table: EncryptedTable::IdempotencyRecords,
                key_version: 3,
                row_count: 1,
            }],
        };
        let error = ensure_keyring_covers_references(&master_key, &missing).unwrap_err();
        assert_eq!(
            error.to_string(),
            "mounted master-key keyring is missing referenced version 3"
        );
    }

    #[tokio::test]
    async fn background_shutdown_waits_for_later_tasks_concurrently() {
        let completed = Arc::new(AtomicUsize::new(0));
        let later_completed = Arc::clone(&completed);
        let blocking_task = tokio::spawn(async {
            std::future::pending::<()>().await;
        });
        let later_task = tokio::spawn(async move {
            tokio::time::sleep(Duration::from_millis(10)).await;
            later_completed.fetch_add(1, Ordering::AcqRel);
        });

        stop_background_tasks(vec![blocking_task, later_task], Duration::from_millis(100)).await;

        assert_eq!(completed.load(Ordering::Acquire), 1);
    }

    #[tokio::test]
    async fn coordinated_shutdown_keeps_background_tasks_alive_while_http_drains() {
        let (listener_shutdown, listener_receiver) = watch::channel(false);
        let listener_observer = listener_receiver.clone();
        let (background_shutdown, background_receiver) = watch::channel(false);
        let (drain_started, drain_started_receiver) = tokio::sync::oneshot::channel();
        let (release_drain, release_receiver) = watch::channel(false);

        let public_listener = listener_receiver.clone();
        let public_release = release_receiver.clone();
        let public_server = async move {
            wait_for_shutdown(public_listener).await;
            let _ = drain_started.send(());
            wait_for_shutdown(public_release).await;
        };
        let observability_server = async move {
            wait_for_shutdown(listener_receiver).await;
            wait_for_shutdown(release_receiver).await;
        };

        let coordinator = tokio::spawn(coordinate_shutdown(
            public_server,
            observability_server,
            async {},
            listener_shutdown,
            background_shutdown,
        ));
        drain_started_receiver.await.unwrap();

        assert!(*listener_observer.borrow());
        assert!(!*background_receiver.borrow());

        release_drain.send(true).unwrap();
        coordinator.await.unwrap();
        assert!(*background_receiver.borrow());
    }

    #[tokio::test]
    async fn overdue_background_tasks_are_aborted_and_joined() {
        let dropped = Arc::new(AtomicBool::new(false));
        let task_dropped = Arc::clone(&dropped);
        let (started, started_receiver) = tokio::sync::oneshot::channel();
        let task = tokio::spawn(async move {
            let _drop_signal = DropSignal(task_dropped);
            let _ = started.send(());
            std::future::pending::<()>().await;
        });
        started_receiver.await.unwrap();

        stop_background_tasks(vec![task], Duration::ZERO).await;
        assert!(dropped.load(Ordering::Acquire));
    }

    #[tokio::test]
    async fn worker_shutdown_propagates_task_panics() {
        let mut workers = JoinSet::new();
        workers.spawn(async { panic!("worker failed") });

        let error = stop_worker_tasks(&mut workers, Duration::from_secs(1))
            .await
            .unwrap_err();
        assert!(error.is_panic());
    }

    #[tokio::test]
    async fn worker_shutdown_ignores_cancellation_from_its_own_abort() {
        let dropped = Arc::new(AtomicBool::new(false));
        let task_dropped = Arc::clone(&dropped);
        let (started, started_receiver) = tokio::sync::oneshot::channel();
        let mut workers = JoinSet::new();
        workers.spawn(async move {
            let _drop_signal = DropSignal(task_dropped);
            let _ = started.send(());
            std::future::pending::<()>().await;
        });
        started_receiver.await.unwrap();

        stop_worker_tasks(&mut workers, Duration::ZERO)
            .await
            .unwrap();
        assert!(dropped.load(Ordering::Acquire));
    }
}
