use std::{future::Future, sync::Arc, time::Duration};

use futures::future::select_all;
use olp_storage::{
    DistributedLimiter, MasterKey, PersistenceError, PgStore, RuntimeHintSubscriber, UsageEmitter,
};
use tokio::{net::TcpListener, sync::watch, task::JoinHandle};
use tracing::{error, info, warn};

use crate::{
    ApiMode, ApiState, LimiterManager, RuntimeManager, TransportRegistry, create_media_spool,
    reconcile_media_jobs_once, usage_tasks::UsageTaskTracker,
};
use crate::{
    connectors::{load_connector_config, reload_persisted_connectors},
    listener,
};

use super::{
    AppResult, BACKGROUND_SHUTDOWN_TIMEOUT,
    commands::{
        maintenance_supervisor, outbox_supervisor, usage_consumer_supervisor,
        usage_epoch_supervisor,
    },
    config::ServerArgs,
    validation::{
        check_secret_permissions, connect_store, load_bootstrap_token_digest, load_key_hasher,
        load_master_key,
    },
};

pub(super) async fn serve(
    mode: ApiMode,
    args: ServerArgs,
    run_worker_in_process: bool,
) -> AppResult<()> {
    if args.http_max_connections == 0 {
        return Err(
            std::io::Error::other("OLP_HTTP_MAX_CONNECTIONS must be greater than zero").into(),
        );
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
    let usage_runtime = if mode.serves_gateway() {
        args.valkey_url.as_ref().map(|url| {
            // Install usage before cloning state into any autonomous producer.
            let (emitter, receiver) = UsageEmitter::bounded(8_192);
            state.usage = Some(emitter.clone());
            let gateway_instance = format!(
                "{}:{}",
                std::env::var("HOSTNAME").unwrap_or_else(|_| "olp".to_owned()),
                args.listen_addr
            );
            let (writer_shutdown, writer_shutdown_receiver) = watch::channel(false);
            let usage_writer_url = url.clone();
            let writer = tokio::spawn(async move {
                match receiver
                    .run_connecting(&usage_writer_url, "olp:v2:usage", writer_shutdown_receiver)
                    .await
                {
                    Ok(()) => true,
                    Err(error) => {
                        error!(%error, "usage stream writer stopped");
                        false
                    }
                }
            });
            let reporter = tokio::spawn(usage_loss_reporter(
                store.clone(),
                emitter.clone(),
                gateway_instance.clone(),
                background_shutdown_receiver.clone(),
            ));
            UsageRuntime {
                emitter,
                gateway_instance,
                writer_shutdown,
                writer,
                reporter,
            }
        })
    } else {
        None
    };
    let mut background_tasks: Vec<JoinHandle<()>> = Vec::new();
    background_tasks.push(spawn_runtime_poller(
        Arc::clone(&state.runtime),
        store.clone(),
        state.transports.clone(),
        state.master_key.clone(),
        background_shutdown_receiver.clone(),
    ));
    if mode.serves_gateway() {
        let _ = state.usage_tasks.spawn(media_reconciliation_supervisor(
            state.clone(),
            background_shutdown_receiver.clone(),
        ));
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
    let usage_tasks = state.usage_tasks.clone();
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
        usage_tasks.clone(),
        background_shutdown_sender,
    )
    .await;
    if let Some(mut usage) = usage_runtime {
        let (producers_clean, reporter_clean) = tokio::join!(
            usage_tasks.wait(BACKGROUND_SHUTDOWN_TIMEOUT),
            stop_usage_reporter(&mut usage.reporter),
        );
        if !producers_clean {
            warn!("usage-producing tasks did not drain cleanly; leaving the process epoch open");
        }
        if !reporter_clean {
            warn!("usage-loss reporter did not stop cleanly before final checkpointing");
        }
        stop_usage_runtime(store.clone(), usage, producers_clean).await;
    } else if !usage_tasks.wait(BACKGROUND_SHUTDOWN_TIMEOUT).await {
        warn!("detached inference tasks did not drain cleanly");
    }
    stop_background_tasks(background_tasks, BACKGROUND_SHUTDOWN_TIMEOUT).await;
    public_result?;
    observability_result?;
    Ok(())
}

pub(super) async fn coordinate_shutdown<Public, Observability, Signal>(
    public_server: Public,
    observability_server: Observability,
    signal: Signal,
    listener_shutdown: watch::Sender<bool>,
    usage_tasks: UsageTaskTracker,
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
    usage_tasks.close();
    let _ = background_shutdown.send(true);
    (public_result, observability_result)
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

struct UsageRuntime {
    emitter: UsageEmitter,
    gateway_instance: String,
    writer_shutdown: watch::Sender<bool>,
    writer: JoinHandle<bool>,
    reporter: JoinHandle<()>,
}

async fn stop_usage_reporter(reporter: &mut JoinHandle<()>) -> bool {
    match tokio::time::timeout(BACKGROUND_SHUTDOWN_TIMEOUT, &mut *reporter).await {
        Ok(Ok(())) => true,
        Ok(Err(error)) => {
            warn!(%error, "usage-loss reporter task stopped unexpectedly");
            false
        }
        Err(_) => {
            reporter.abort();
            let _ = reporter.await;
            false
        }
    }
}

async fn stop_usage_runtime(store: PgStore, mut usage: UsageRuntime, producers_clean: bool) {
    let _ = usage.writer_shutdown.send(true);
    let writer_clean =
        match tokio::time::timeout(BACKGROUND_SHUTDOWN_TIMEOUT, &mut usage.writer).await {
            Ok(Ok(clean)) => clean,
            Ok(Err(error)) => {
                warn!(%error, "usage stream writer task stopped unexpectedly");
                false
            }
            Err(_) => {
                warn!("usage stream writer did not stop before the shutdown deadline; aborting it");
                usage.writer.abort();
                let _ = usage.writer.await;
                false
            }
        };
    let snapshot = usage.emitter.snapshot();
    let graceful = producers_clean && writer_clean && snapshot.gracefully_drained();
    if !graceful {
        warn!(
            producers_clean,
            writer_clean,
            pending = snapshot.pending(),
            "usage shutdown was incomplete; checkpointing an open process epoch"
        );
    }
    final_usage_checkpoint(&store, &usage.gateway_instance, &snapshot, graceful).await;
}

async fn final_usage_checkpoint(
    store: &PgStore,
    gateway_instance: &str,
    snapshot: &olp_storage::UsageBufferSnapshot,
    graceful: bool,
) {
    let deadline = tokio::time::Instant::now() + Duration::from_secs(4);
    let mut backoff = Duration::from_millis(100);
    loop {
        let checkpoint = async {
            if graceful {
                store
                    .close_usage_buffer_epoch(gateway_instance, snapshot)
                    .await
            } else {
                store
                    .report_usage_buffer_loss(gateway_instance, snapshot)
                    .await
            }
        };
        match tokio::time::timeout_at(deadline, checkpoint).await {
            Ok(Ok(_)) => return,
            Ok(Err(PersistenceError::Database(error)))
                if tokio::time::Instant::now() < deadline =>
            {
                warn!(%error, %gateway_instance, "final usage-loss checkpoint failed; retrying");
            }
            Ok(Err(error)) => {
                error!(%error, %gateway_instance, lost = snapshot.lost(), "final usage-loss checkpoint could not be persisted");
                return;
            }
            Err(_) => {
                error!(%gateway_instance, lost = snapshot.lost(), "final usage-loss checkpoint timed out");
                return;
            }
        }
        if tokio::time::timeout_at(deadline, tokio::time::sleep(backoff))
            .await
            .is_err()
        {
            error!(%gateway_instance, lost = snapshot.lost(), "final usage-loss checkpoint timed out");
            return;
        }
        backoff = (backoff * 2).min(Duration::from_millis(500));
    }
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
                    return;
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
