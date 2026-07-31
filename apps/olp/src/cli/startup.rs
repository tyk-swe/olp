use std::{
    collections::BTreeSet, future::Future, panic::AssertUnwindSafe, sync::Arc, time::Duration,
};

use futures::{FutureExt as _, future::select_all};
use olp_storage::{
    DistributedLimiter, MasterKey, PgStore, REQUEST_METADATA_STREAM, RequestMetadataEmitter,
    RuntimeHintSubscriber,
};
use tokio::{
    net::TcpListener,
    sync::{mpsc, watch},
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
        maintenance_supervisor, outbox_supervisor, request_metadata_consumer_supervisor,
        request_metadata_epoch_supervisor,
    },
    config::ServeArgs,
    validation::{
        check_secret_permissions, connect_store, load_auth_hmac_key, load_bootstrap_token_digest,
        load_master_key,
    },
};

pub(super) struct BackgroundTaskStatus {
    name: &'static str,
    result: AppResult<()>,
}

pub(super) fn spawn_fallible_background_task<F>(
    name: &'static str,
    future: F,
    status: mpsc::UnboundedSender<BackgroundTaskStatus>,
    shutdown: watch::Receiver<bool>,
) -> JoinHandle<()>
where
    F: Future<Output = AppResult<()>> + Send + 'static,
{
    tokio::spawn(async move {
        let result = AssertUnwindSafe(future)
            .catch_unwind()
            .await
            .unwrap_or_else(|_| {
                Err(std::io::Error::other(format!("background task `{name}` panicked")).into())
            });
        if result.is_err() || !*shutdown.borrow() {
            let _ = status.send(BackgroundTaskStatus { name, result });
        }
    })
}

pub(super) fn spawn_background_task<F>(
    name: &'static str,
    future: F,
    status: mpsc::UnboundedSender<BackgroundTaskStatus>,
    shutdown: watch::Receiver<bool>,
) -> JoinHandle<()>
where
    F: Future<Output = ()> + Send + 'static,
{
    spawn_fallible_background_task(
        name,
        async move {
            future.await;
            Ok(())
        },
        status,
        shutdown,
    )
}

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
        .assets
        .media_spool_dir
        .clone()
        .unwrap_or_else(std::env::temp_dir);
    let media_spool = create_media_spool(&media_spool_dir, args.assets.media_spool_capacity_bytes)?;
    let mut state = ApiState::new_with_media_spool(
        mode,
        Some(store.clone()),
        runtime,
        args.public_origin.as_str(),
        args.assets.console_dir,
        media_spool,
    );
    state.set_public_admission_limits(
        args.http_max_in_flight_inference_requests,
        args.http_max_in_flight_management_requests,
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
    // The E2E harness points providers at a loopback mock upstream. Same
    // posture as the OIDC override above: compiled out of release binaries.
    #[cfg(all(debug_assertions, feature = "test-util"))]
    if crate::provider_adapter::insecure_provider_endpoints_for_tests() {
        warn!("test-only insecure provider endpoints are enabled");
    }
    if let Some(path) = &args.auth_hmac_key_file {
        check_secret_permissions(path).await?;
        state.auth_hmac_key = Some(Arc::new(load_auth_hmac_key(path).await?));
    }
    state.set_trusted_proxy_cidrs(args.trusted_proxy_cidrs.0.clone());
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
    if let Some(path) = &args.assets.connector_config_file {
        register_mounted_connectors(path, &state.transports).await?;
    }
    match activate_latest_runtime(
        &state.runtime,
        &store,
        &state.transports,
        &state.circuits,
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
    let (background_status_sender, mut background_status_receiver) = mpsc::unbounded_channel();
    let mut background_tasks: Vec<JoinHandle<()>> = Vec::new();
    background_tasks.push(spawn_background_task(
        "runtime poller",
        runtime_poller(
            Arc::clone(&state.runtime),
            store.clone(),
            state.transports.clone(),
            state.circuits.clone(),
            state.master_key.clone(),
            background_shutdown_receiver.clone(),
        ),
        background_status_sender.clone(),
        background_shutdown_receiver.clone(),
    ));
    if let Some(url) = &args.valkey_url {
        background_tasks.push(spawn_background_task(
            "runtime hint subscriber",
            runtime_hint_supervisor(
                Arc::clone(&state.runtime),
                store.clone(),
                state.transports.clone(),
                state.circuits.clone(),
                state.master_key.clone(),
                url.clone(),
                background_shutdown_receiver.clone(),
            ),
            background_status_sender.clone(),
            background_shutdown_receiver.clone(),
        ));
        state.limiter.mark_configured();
        background_tasks.push(spawn_background_task(
            "distributed limiter",
            limiter_supervisor(
                state.limiter.clone(),
                url.clone(),
                background_shutdown_receiver.clone(),
            ),
            background_status_sender.clone(),
            background_shutdown_receiver.clone(),
        ));

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
            background_tasks.push(spawn_background_task(
                "request metadata loss reporter",
                request_metadata_loss_reporter(
                    store.clone(),
                    emitter,
                    gateway_instance,
                    background_shutdown_receiver.clone(),
                ),
                background_status_sender.clone(),
                background_shutdown_receiver.clone(),
            ));
            let request_metadata_writer_url = url.clone();
            let request_metadata_writer_shutdown = background_shutdown_receiver.clone();
            let request_metadata_stream_max_length = args.request_metadata_stream_max_length;
            background_tasks.push(spawn_fallible_background_task(
                "request metadata stream writer",
                async move {
                    let result: AppResult<()> = receiver
                        .run_connecting(
                            &request_metadata_writer_url,
                            REQUEST_METADATA_STREAM,
                            request_metadata_stream_max_length,
                            request_metadata_writer_shutdown,
                        )
                        .await
                        .map_err(Into::into);
                    if let Err(error) = &result {
                        error!(%error, "request metadata stream writer stopped");
                    }
                    result
                },
                background_status_sender.clone(),
                background_shutdown_receiver.clone(),
            ));
        }
        if run_worker_in_process {
            background_tasks.push(spawn_background_task(
                "outbox publisher",
                outbox_supervisor(
                    store.clone(),
                    url.clone(),
                    background_shutdown_receiver.clone(),
                ),
                background_status_sender.clone(),
                background_shutdown_receiver.clone(),
            ));
            background_tasks.push(spawn_background_task(
                "request metadata consumer",
                request_metadata_consumer_supervisor(
                    store.clone(),
                    url.clone(),
                    background_shutdown_receiver.clone(),
                ),
                background_status_sender.clone(),
                background_shutdown_receiver.clone(),
            ));
        }
    }
    if run_worker_in_process {
        background_tasks.push(spawn_background_task(
            "maintenance worker",
            maintenance_supervisor(store.clone(), background_shutdown_receiver.clone()),
            background_status_sender.clone(),
            background_shutdown_receiver.clone(),
        ));
        background_tasks.push(spawn_background_task(
            "request metadata epoch worker",
            request_metadata_epoch_supervisor(store.clone(), background_shutdown_receiver.clone()),
            background_status_sender.clone(),
            background_shutdown_receiver.clone(),
        ));
    }
    let dependencies = state.mode_dependencies()?;
    let observability_state = dependencies.observability();
    if let Some(gateway_state) = dependencies.gateway() {
        background_tasks.push(spawn_background_task(
            "media reconciliation",
            media_reconciliation_supervisor(gateway_state, background_shutdown_receiver.clone()),
            background_status_sender.clone(),
            background_shutdown_receiver.clone(),
        ));
    }
    background_tasks.push(spawn_background_task(
        "observability cache",
        crate::run_observability_cache(
            observability_state.clone(),
            background_shutdown_receiver.clone(),
        ),
        background_status_sender.clone(),
        background_shutdown_receiver.clone(),
    ));
    drop(background_status_sender);

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
        shutdown_reason(shutdown_signal(), &mut background_status_receiver),
        listener_shutdown_sender,
        background_shutdown_sender,
    )
    .await;
    stop_background_tasks(background_tasks, BACKGROUND_SHUTDOWN_TIMEOUT).await;
    let terminal_error =
        terminal_error.or_else(|| background_task_error(&mut background_status_receiver));
    public_result?;
    observability_result?;
    if let Some(error) = terminal_error {
        return Err(error);
    }
    Ok(())
}

pub(super) async fn shutdown_reason<Signal>(
    signal: Signal,
    background_status: &mut mpsc::UnboundedReceiver<BackgroundTaskStatus>,
) -> Option<AppError>
where
    Signal: Future<Output = ()>,
{
    tokio::select! {
        biased;
        status = background_status.recv() => match status {
            Some(BackgroundTaskStatus { result: Err(error), .. }) => Some(error),
            Some(BackgroundTaskStatus { name, result: Ok(()) }) => Some(std::io::Error::other(
                format!("background task `{name}` stopped unexpectedly"),
            ).into()),
            None => Some(std::io::Error::other(
                "background task monitors stopped unexpectedly",
            ).into()),
        },
        () = signal => None,
    }
}

pub(super) fn background_task_error(
    background_status: &mut mpsc::UnboundedReceiver<BackgroundTaskStatus>,
) -> Option<AppError> {
    let status = background_status.try_recv().ok()?;
    Some(match status.result {
        Err(error) => error,
        Ok(()) => std::io::Error::other(format!(
            "background task `{}` stopped unexpectedly",
            status.name
        ))
        .into(),
    })
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
    circuits: crate::circuit::CircuitBreaker,
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
                            &circuits,
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

async fn runtime_poller(
    runtime: Arc<RuntimeManager>,
    store: PgStore,
    transports: TransportRegistry,
    circuits: crate::circuit::CircuitBreaker,
    master_key: Option<Arc<MasterKey>>,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut interval = tokio::time::interval(Duration::from_secs(5));
    interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! {
            _ = interval.tick() => {
                match activate_latest_runtime(
                    &runtime,
                    &store,
                    &transports,
                    &circuits,
                    master_key.as_deref(),
                )
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

const MAX_RUNTIME_REJECTION_DIAGNOSTICS: usize = 32;

fn record_runtime_rejection(
    rejected: &mut Vec<String>,
    rejected_count: &mut usize,
    sequence: i64,
    error: &dyn std::fmt::Display,
) {
    *rejected_count = (*rejected_count).saturating_add(1);
    if rejected.len() < MAX_RUNTIME_REJECTION_DIAGNOSTICS {
        rejected.push(format!("{sequence}: {error}"));
    }
}

async fn activate_latest_runtime(
    runtime: &RuntimeManager,
    store: &PgStore,
    transports: &TransportRegistry,
    circuits: &crate::circuit::CircuitBreaker,
    master_key: Option<&MasterKey>,
) -> AppResult<bool> {
    const PAGE_SIZE: u16 = 32;
    let installed_sequence = runtime.active_generation_ordinal();
    let mut before_sequence = None;
    let mut current_api_keys = None;
    let mut rejected = Vec::new();
    let mut rejected_count = 0usize;
    loop {
        let releases = store
            .valid_runtime_releases_before(PAGE_SIZE, installed_sequence, before_sequence)
            .await?;
        if releases.is_empty() {
            break;
        }
        let page_exhausted = releases.len() < usize::from(PAGE_SIZE);
        before_sequence = releases.last().map(|release| release.sequence);
        if current_api_keys.is_none() {
            current_api_keys = Some(store.current_runtime_api_keys().await?);
        }
        let current_api_keys = current_api_keys
            .as_ref()
            .expect("runtime API keys were loaded above");
        for release in releases {
            let snapshot =
                match runtime.decode_release_candidate(&release, current_api_keys.clone()) {
                    Ok(snapshot) => snapshot,
                    Err(error) => {
                        record_runtime_rejection(
                            &mut rejected,
                            &mut rejected_count,
                            release.sequence,
                            &error,
                        );
                        continue;
                    }
                };
            // Provider transports are assembled from normalized secret storage, not
            // the public runtime payload. Require the release-time sidecar to match
            // every current transport-affecting field before accepting an LKG.
            if let Err(error) = store.runtime_provider_configurations(&snapshot).await {
                record_runtime_rejection(
                    &mut rejected,
                    &mut rejected_count,
                    release.sequence,
                    &error,
                );
                continue;
            }
            let mut candidate_transports = transports.snapshot();
            if let Some(master_key) = master_key
                && let Err(error) =
                    load_runtime_transports(store, master_key, &snapshot, &mut candidate_transports)
                        .await
            {
                record_runtime_rejection(
                    &mut rejected,
                    &mut rejected_count,
                    release.sequence,
                    &error,
                );
                continue;
            }
            candidate_transports
                .retain(|provider_id, _| snapshot.providers.contains_key(provider_id));
            let live_targets = snapshot
                .routes
                .values()
                .flat_map(|route| route.targets.iter().map(|target| target.id))
                .collect::<BTreeSet<_>>();
            match runtime.install(snapshot, candidate_transports) {
                Ok(installed) => {
                    if installed {
                        circuits.retain_targets(&live_targets);
                    }
                    if !rejected.is_empty() {
                        warn!(
                            rejected = ?rejected,
                            rejected_count,
                            selected_sequence = release.sequence,
                            "installed previous verified runtime release after rejecting newer candidates"
                        );
                    }
                    return Ok(installed);
                }
                Err(error) => record_runtime_rejection(
                    &mut rejected,
                    &mut rejected_count,
                    release.sequence,
                    &error,
                ),
            }
        }
        if page_exhausted {
            break;
        }
    }
    if rejected_count == 0 {
        return Ok(false);
    }
    let omitted = rejected_count.saturating_sub(rejected.len());
    let omitted = if omitted == 0 {
        String::new()
    } else {
        format!("; {omitted} more omitted")
    };
    Err(std::io::Error::other(format!(
        "no verified runtime release could be installed: {}{omitted}",
        rejected.join("; "),
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

#[cfg(test)]
mod rejection_tests {
    use super::{MAX_RUNTIME_REJECTION_DIAGNOSTICS, record_runtime_rejection};

    #[test]
    fn runtime_rejection_diagnostics_are_bounded() {
        let mut rejected = Vec::new();
        let mut count = 0;
        for sequence in 0..=MAX_RUNTIME_REJECTION_DIAGNOSTICS as i64 {
            record_runtime_rejection(&mut rejected, &mut count, sequence, &"invalid");
        }
        assert_eq!(count, MAX_RUNTIME_REJECTION_DIAGNOSTICS + 1);
        assert_eq!(rejected.len(), MAX_RUNTIME_REJECTION_DIAGNOSTICS);
    }
}
