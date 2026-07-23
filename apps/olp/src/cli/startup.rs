use std::{future::Future, sync::Arc, time::Duration};

use futures::future::select_all;
use olp_storage::{
    DistributedLimiter, MasterKey, PgStore, REQUEST_METADATA_STREAM, RequestMetadataEmitter,
    RuntimeHintSubscriber,
};
use tokio::{
    net::TcpListener,
    sync::{oneshot, watch},
    task::JoinHandle,
};
use tracing::{error, info, warn};

use crate::{
    ApiMode, ApiState, ReloadableLimiter, RuntimeManager, TransportRegistry, create_media_spool,
    reconcile_media_jobs_once,
};
use crate::{
    connectors::{load_runtime_transports, register_mounted_connectors},
    listener,
};

use super::{
    AppError, AppResult, BACKGROUND_SHUTDOWN_TIMEOUT,
    commands::{
        maintenance_supervisor, outbox_supervisor, preflight_request_metadata_stream_or_defer,
        request_metadata_consumer_supervisor, request_metadata_epoch_supervisor,
    },
    config::ServeArgs,
    validation::{
        check_secret_permissions, connect_store, load_auth_hmac_key, load_bootstrap_token_digest,
        load_master_key,
    },
};

pub(super) async fn serve(
    mode: ApiMode,
    args: ServeArgs,
    run_worker_in_process: bool,
) -> AppResult<()> {
    if args.http_max_connections == 0 {
        return Err(
            std::io::Error::other("OLP_HTTP_MAX_CONNECTIONS must be greater than zero").into(),
        );
    }
    if args.auth_hmac_key_file.is_none() {
        return Err(std::io::Error::other(
            "OLP_AUTH_HMAC_KEY_FILE is required when serving an HTTP mode",
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
        args.public_origin.as_str(),
        args.console_dir,
        media_spool,
    );
    state.local_login_enabled = args.local_login_enabled;
    // The browser integration fixture uses a loopback mock identity
    // provider. This branch is compiled out of release binaries, so no
    // deployment setting can weaken the production HTTPS/SSRF policy.
    #[cfg(debug_assertions)]
    if std::env::var("OLP_ALLOW_INSECURE_OIDC_FOR_TESTS").as_deref() == Ok("test-only") {
        state.oidc_allow_insecure_test_endpoints = true;
        warn!("test-only loopback OIDC endpoints are enabled");
    }
    if let Some(path) = &args.auth_hmac_key_file {
        check_secret_permissions(path).await?;
        state.auth_hmac_key = Some(Arc::new(load_auth_hmac_key(path).await?));
    }
    state.set_trusted_proxy_cidrs(args.trusted_proxy_cidrs.clone());
    let setup_required = if mode.serves_control() {
        store.setup_required().await?
    } else {
        false
    };
    let bootstrap_token_digest = if let Some(path) = &args.bootstrap_token_file {
        check_secret_permissions(path).await?;
        let auth_hmac_key = state.auth_hmac_key.as_deref().ok_or_else(|| {
            std::io::Error::other(
                "OLP_BOOTSTRAP_TOKEN_FILE requires OLP_AUTH_HMAC_KEY_FILE for digest verification",
            )
        })?;
        Some(load_bootstrap_token_digest(path, auth_hmac_key).await?)
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
        register_mounted_connectors(path, &state.transports).await?;
    }
    match activate_latest_runtime(
        &state.runtime,
        &store,
        &state.transports,
        state.master_key.as_deref(),
    )
    .await
    {
        Ok(true) => info!(
            generation = ?state.runtime.active_generation_ordinal(),
            "loaded runtime generation"
        ),
        Ok(false) => warn!("no active runtime generation; gateway will remain unready"),
        Err(error) => error!(%error, "initial runtime release was rejected"),
    }
    let listener = TcpListener::bind(args.listen_addr).await?;
    let observability_listener = TcpListener::bind(args.observability_listen_addr).await?;
    let (background_shutdown_sender, background_shutdown_receiver) = watch::channel(false);
    let (listener_shutdown_sender, listener_shutdown_receiver) = watch::channel(false);
    let mut request_metadata_writer_status = None;
    let mut background_tasks: Vec<JoinHandle<()>> = Vec::new();
    background_tasks.push(spawn_runtime_poller(
        Arc::clone(&state.runtime),
        store.clone(),
        state.transports.clone(),
        state.master_key.clone(),
        background_shutdown_receiver.clone(),
    ));
    if let Some(url) = &args.valkey_url {
        if mode.serves_gateway() || run_worker_in_process {
            preflight_request_metadata_stream_or_defer(url).await?;
        }
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
            let (emitter, receiver) = RequestMetadataEmitter::bounded(8_192);
            state.request_metadata = Some(emitter.clone());
            let gateway_instance = format!(
                "{}:{}",
                std::env::var("HOSTNAME").unwrap_or_else(|_| "olp".to_owned()),
                args.listen_addr
            );
            background_tasks.push(tokio::spawn(request_metadata_loss_reporter(
                store.clone(),
                emitter,
                gateway_instance,
                background_shutdown_receiver.clone(),
            )));
            let request_metadata_writer_url = url.clone();
            let request_metadata_writer_shutdown = background_shutdown_receiver.clone();
            let (status_sender, status_receiver) = oneshot::channel();
            request_metadata_writer_status = Some(status_receiver);
            background_tasks.push(tokio::spawn(async move {
                let result: AppResult<()> = receiver
                    .run_connecting(
                        &request_metadata_writer_url,
                        REQUEST_METADATA_STREAM,
                        request_metadata_writer_shutdown,
                    )
                    .await
                    .map_err(Into::into);
                if let Err(error) = &result {
                    error!(%error, "request metadata stream writer stopped");
                }
                let _ = status_sender.send(result);
            }));
        }
        if run_worker_in_process {
            background_tasks.push(tokio::spawn(outbox_supervisor(
                store.clone(),
                url.clone(),
                background_shutdown_receiver.clone(),
            )));
            background_tasks.push(tokio::spawn(request_metadata_consumer_supervisor(
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
        background_tasks.push(tokio::spawn(request_metadata_epoch_supervisor(
            store.clone(),
            background_shutdown_receiver.clone(),
        )));
    }
    let dependencies = state.mode_dependencies()?;
    let observability_state = dependencies.observability();
    if let Some(gateway_state) = dependencies.gateway() {
        background_tasks.push(tokio::spawn(media_reconciliation_supervisor(
            gateway_state,
            background_shutdown_receiver.clone(),
        )));
    }
    background_tasks.push(crate::spawn_observability_cache(
        observability_state.clone(),
        background_shutdown_receiver.clone(),
    ));

    info!(address = %args.listen_addr, ?mode, "OLP public listener ready");
    info!(address = %args.observability_listen_addr, ?mode, "OLP observability listener ready");
    let public_server = listener::serve_http(
        listener,
        crate::router::validated_public_router(dependencies),
        listener::HttpServerConfig::standard(args.http_max_connections),
        listener_shutdown_receiver.clone(),
    );
    // This listener has its own router-level concurrency cap. Constrain its
    // connection envelope too so metrics traffic cannot occupy the public
    // listener's entire process-level resource budget.
    let observability_server = listener::serve_http(
        observability_listener,
        crate::observability_router(observability_state),
        listener::HttpServerConfig::standard(args.http_max_connections.clamp(1, 32)),
        listener_shutdown_receiver,
    );
    let (public_result, observability_result, terminal_error) = coordinate_shutdown(
        public_server,
        observability_server,
        shutdown_reason(shutdown_signal(), request_metadata_writer_status.as_mut()),
        listener_shutdown_sender,
        background_shutdown_sender,
    )
    .await;
    stop_background_tasks(background_tasks, BACKGROUND_SHUTDOWN_TIMEOUT).await;
    let terminal_error =
        resolve_request_metadata_writer_error(request_metadata_writer_status, terminal_error).await;
    public_result?;
    observability_result?;
    if let Some(error) = terminal_error {
        return Err(error);
    }
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

async fn media_reconciliation_supervisor(
    state: crate::GatewayState,
    mut shutdown: watch::Receiver<bool>,
) {
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

async fn request_metadata_loss_reporter(
    store: PgStore,
    emitter: RequestMetadataEmitter,
    gateway_instance: String,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut interval = tokio::time::interval(Duration::from_secs(1));
    interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! {
            _ = interval.tick() => {
                let snapshot = emitter.snapshot();
                if let Err(error) = store.report_request_metadata_buffer_loss(&gateway_instance, &snapshot).await {
                    warn!(%error, %gateway_instance, "request metadata loss checkpoint failed; retrying");
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
                        match store.close_request_metadata_buffer_epoch(&gateway_instance, &snapshot).await {
                            Ok(_) => return,
                            Err(error) if tokio::time::Instant::now() < deadline => {
                                warn!(%error, %gateway_instance, "final request metadata loss checkpoint failed; retrying");
                                tokio::time::sleep(Duration::from_millis(200)).await;
                            }
                            Err(error) => {
                                error!(%error, %gateway_instance, lost = snapshot.lost(), "final request metadata loss checkpoint could not be persisted");
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
                            Ok(true) => info!(
                                generation = ?runtime.active_generation_ordinal(),
                                "runtime hint activated generation"
                            ),
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
                            info!(
                                generation = ?runtime.active_generation_ordinal(),
                                "runtime generation activated"
                            );
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
    reloadable_limiter: ReloadableLimiter,
    valkey_url: String,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut backoff = Duration::from_millis(100);
    loop {
        if *shutdown.borrow() {
            return;
        }
        if let Some(limiter) = reloadable_limiter.current() {
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
            reloadable_limiter.clear();
            warn!("Valkey limiter health check failed; hard limits remain fail-closed");
        }

        match tokio::time::timeout(
            Duration::from_secs(3),
            DistributedLimiter::connect(&valkey_url, "olp:v2:limits"),
        )
        .await
        {
            Ok(Ok(limiter)) => {
                reloadable_limiter.install(limiter);
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
    let releases = store
        .recent_valid_runtime_releases_after(32, runtime.active_generation_ordinal())
        .await?;
    if releases.is_empty() {
        return Ok(false);
    }
    let current_api_keys = store.current_runtime_api_keys().await?;
    let mut rejected = Vec::new();
    for release in releases {
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
        if let Err(error) = store.runtime_provider_configurations(&snapshot).await {
            rejected.push(format!("{}: {error}", release.sequence));
            continue;
        }
        let mut candidate_transports = transports.snapshot();
        if let Some(master_key) = master_key
            && let Err(error) =
                load_runtime_transports(store, master_key, &snapshot, &mut candidate_transports)
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
