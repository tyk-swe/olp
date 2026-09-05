use std::{sync::Arc, time::Duration};

use olp_db::request_metadata::writer::run_connecting;
use olp_db::{security::key_material::AuthHmacKey, store::Store, valkey::Keyspace};
use olp_engine::inference::request_metadata::Emitter;
use tokio::{
    net::TcpListener,
    sync::{oneshot, watch},
    task::JoinHandle,
};
use tracing::{error, info, warn};

use olp_engine::inference::runtime::Manager;

use crate::{
    application::mode::ApiMode, bootstrap::state::ProcessComposition, media_spool,
    observability::tracing::RuntimeConfig as TracingRuntimeConfig,
};
use crate::{bootstrap::connectors::register_mounted_connectors, public_http::listener};

use {
    super::{
        AppResult, BACKGROUND_SHUTDOWN_TIMEOUT,
        config::ServeArgs,
        lifecycle::{
            coordinate_shutdown, resolve_request_metadata_writer_error, shutdown_reason,
            shutdown_signal, stop_background_tasks,
        },
        runtime_activation::{
            RuntimeActivator, RuntimeHintSource, activate_latest_runtime, runtime_hint_supervisor,
            spawn_runtime_poller,
        },
        validation::{
            connect_store, load_auth_hmac_key, load_bootstrap_token_digest, load_master_key,
        },
    },
    crate::{
        application::secret_files::check_secret_permissions,
        bootstrap::workers::{
            costs::cost_reconciliation_supervisor,
            maintenance::maintenance_supervisor,
            outbox::outbox_supervisor,
            request_metadata::{
                request_metadata_consumer_name, request_metadata_consumer_supervisor,
                request_metadata_epoch_supervisor,
            },
            service_supervisors::{
                limiter_supervisor, limits_policy_supervisor, load_limits_outage_policy,
                media_reconciliation_supervisor, request_metadata_loss_reporter,
            },
        },
    },
};

struct BackgroundPlane {
    tasks: Vec<JoinHandle<()>>,
    request_metadata_writer_status: Option<oneshot::Receiver<AppResult<()>>>,
}

pub(super) async fn serve(
    mode: ApiMode,
    args: ServeArgs,
    run_worker_in_process: bool,
    tracing: Option<TracingRuntimeConfig>,
) -> AppResult<()> {
    let (store, mut state) = prepare_state(mode, &args, tracing).await?;
    let listener = TcpListener::bind(args.listen_addr).await?;
    let observability_listener = TcpListener::bind(args.observability_listen_addr).await?;
    let (background_shutdown_sender, background_shutdown_receiver) = watch::channel(false);
    let (listener_shutdown_sender, listener_shutdown_receiver) = watch::channel(false);
    let mut plane = spawn_background_plane(
        &mut state,
        &args,
        &store,
        mode,
        run_worker_in_process,
        &background_shutdown_receiver,
    )
    .await?;
    let dependencies = state.mode_dependencies();
    let observability_state = dependencies.observability();
    if let Some(gateway_state) = dependencies.gateway() {
        plane
            .tasks
            .push(tokio::spawn(media_reconciliation_supervisor(
                gateway_state.media_jobs,
                background_shutdown_receiver.clone(),
            )));
    }
    plane
        .tasks
        .push(crate::observability::cache::spawn_observability_cache(
            observability_state.clone(),
            background_shutdown_receiver.clone(),
        ));

    info!(address = %args.listen_addr, ?mode, "OLP public listener ready");
    info!(address = %args.observability_listen_addr, ?mode, "OLP observability listener ready");
    let connection_max_age = Duration::from_secs(args.http_connection_max_age_seconds);
    let connection_drain_timeout = Duration::from_secs(args.http_connection_drain_timeout_seconds);
    let public_server = listener::serve_http(
        listener,
        crate::public_http::router::validated_public_router(dependencies),
        listener::HttpServerConfig::standard(
            args.http_max_connections,
            connection_max_age,
            connection_drain_timeout,
        ),
        listener_shutdown_receiver.clone(),
    );
    // This listener has its own router-level concurrency cap. Constrain its
    // connection envelope too so metrics traffic cannot occupy the public
    // listener's entire process-level resource budget.
    let observability_server = listener::serve_http(
        observability_listener,
        crate::observability::router(observability_state),
        listener::HttpServerConfig::standard(
            args.http_max_connections.clamp(1, 32),
            connection_max_age,
            connection_drain_timeout,
        ),
        listener_shutdown_receiver,
    );
    let (public_result, observability_result, terminal_error) = coordinate_shutdown(
        public_server,
        observability_server,
        shutdown_reason(
            shutdown_signal(),
            plane.request_metadata_writer_status.as_mut(),
        ),
        listener_shutdown_sender,
        background_shutdown_sender,
    )
    .await;
    stop_background_tasks(plane.tasks, BACKGROUND_SHUTDOWN_TIMEOUT).await;
    let terminal_error =
        resolve_request_metadata_writer_error(plane.request_metadata_writer_status, terminal_error)
            .await;
    public_result?;
    observability_result?;
    if let Some(error) = terminal_error {
        return Err(error);
    }
    Ok(())
}

async fn prepare_state(
    mode: ApiMode,
    args: &ServeArgs,
    tracing: Option<TracingRuntimeConfig>,
) -> AppResult<(Store, ProcessComposition)> {
    validate_serve_args(args)?;
    let store = connect_store(&args.database).await?;
    let auth_hmac_key = load_serve_auth_hmac_key(args).await?;
    let request_tracing = match tracing {
        Some(runtime) => Some(runtime.for_installation(store.installation_id().await?)),
        None => None,
    };
    let mut state = compose_state(mode, args, &store, auth_hmac_key, request_tracing)?;
    apply_secrets_and_policy(&mut state, args, &store, mode).await?;
    if let Some(path) = &args.assets.connector_config_file {
        register_mounted_connectors(
            path,
            &state.transports,
            &state.provider_egress_policy,
            state.provider_response_limits,
        )
        .await?;
    }
    activate_initial_runtime(&state, &store).await;
    Ok((store, state))
}

fn validate_serve_args(args: &ServeArgs) -> AppResult<()> {
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
    Ok(())
}

/// Every HTTP mode authenticates with the HMAC key, so it is loaded before
/// the process composition exists rather than patched in afterwards.
async fn load_serve_auth_hmac_key(args: &ServeArgs) -> AppResult<Arc<AuthHmacKey>> {
    let path = args.auth_hmac_key_file.as_ref().ok_or_else(|| {
        std::io::Error::other("OLP_AUTH_HMAC_KEY_FILE is required when serving an HTTP mode")
    })?;
    check_secret_permissions(path).await?;
    Ok(Arc::new(load_auth_hmac_key(path).await?))
}

fn compose_state(
    mode: ApiMode,
    args: &ServeArgs,
    store: &Store,
    auth_hmac_key: Arc<AuthHmacKey>,
    request_tracing: Option<crate::observability::tracing::RequestConfig>,
) -> AppResult<ProcessComposition> {
    let runtime = Arc::new(Manager::empty());
    let media_spool_dir = args
        .assets
        .media_spool_dir
        .clone()
        .unwrap_or_else(std::env::temp_dir);
    let media_spool =
        media_spool::create(&media_spool_dir, args.assets.media_spool_capacity_bytes)?;
    let body_limits = args
        .body_limits
        .limits()
        .validate(args.assets.media_spool_capacity_bytes)
        .map_err(std::io::Error::other)?;
    let provider_response_limits = args
        .provider_response_limits
        .limits()
        .map_err(std::io::Error::other)?;
    let mut state = ProcessComposition::new_with_media_spool(
        mode,
        store.clone(),
        runtime,
        auth_hmac_key,
        args.public_origin.as_str(),
        args.assets.console_dir.clone(),
        media_spool,
    );
    state.set_public_admission_limits(
        args.http_max_in_flight_inference_requests,
        args.http_max_in_flight_management_requests,
    );
    state.local_login_enabled = args.local_login_enabled;
    state.request_tracing = request_tracing;
    state.body_limits = body_limits;
    state.provider_response_limits = provider_response_limits;
    // The browser integration fixture uses a loopback mock identity
    // provider. This branch is compiled out of release binaries, so no
    // deployment setting can weaken the production HTTPS/SSRF policy.
    #[cfg(debug_assertions)]
    if std::env::var("OLP_ALLOW_INSECURE_OIDC_FOR_TESTS").as_deref() == Ok("test-only") {
        state.oidc_allow_insecure_test_endpoints = true;
        warn!("test-only loopback OIDC endpoints are enabled");
    }
    let provider_egress_policy = args.provider_egress.policy();
    if !provider_egress_policy.allowed_networks().is_empty()
        || !provider_egress_policy.plain_http_hosts().is_empty()
    {
        warn!(
            allowed_networks = ?provider_egress_policy.allowed_networks(),
            plain_http_hosts = ?provider_egress_policy.plain_http_hosts(),
            "provider egress policy exempts non-public or plain-HTTP endpoints"
        );
    }
    state.set_provider_egress_policy(provider_egress_policy);
    Ok(state)
}

async fn apply_secrets_and_policy(
    state: &mut ProcessComposition,
    args: &ServeArgs,
    store: &Store,
    mode: ApiMode,
) -> AppResult<()> {
    state.set_trusted_proxy_cidrs(args.trusted_proxy_cidrs.0.clone());
    state.set_gateway_cors_allowed_origins(args.gateway_cors_allowed_origins.0.clone());
    let setup_required = if mode.serves_control() {
        store.setup_required().await?
    } else {
        false
    };
    let bootstrap_token_digest = if let Some(path) = &args.bootstrap_token_file {
        check_secret_permissions(path).await?;
        let auth_hmac_key = &*state.auth_hmac_key;
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
    Ok(())
}

async fn activate_initial_runtime(state: &ProcessComposition, store: &Store) {
    match activate_latest_runtime(
        &state.runtime,
        store,
        &state.transports,
        &state.circuits,
        state.master_key.as_deref(),
        &state.provider_egress_policy,
        state.provider_response_limits,
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
}

async fn spawn_background_plane(
    state: &mut ProcessComposition,
    args: &ServeArgs,
    store: &Store,
    mode: ApiMode,
    run_worker_in_process: bool,
    shutdown: &watch::Receiver<bool>,
) -> AppResult<BackgroundPlane> {
    let mut plane = BackgroundPlane {
        tasks: Vec::new(),
        request_metadata_writer_status: None,
    };
    let activator = RuntimeActivator {
        runtime: Arc::clone(&state.runtime),
        store: store.clone(),
        transports: state.transports.clone(),
        circuits: state.circuits.clone(),
        master_key: state.master_key.clone(),
        egress_policy: Arc::clone(&state.provider_egress_policy),
        response_limits: state.provider_response_limits,
    };
    plane
        .tasks
        .push(spawn_runtime_poller(activator.clone(), shutdown.clone()));
    if let Some(url) = &args.valkey_url {
        let keyspace = store.valkey_keyspace().await?;
        plane.tasks.push(tokio::spawn(runtime_hint_supervisor(
            activator.clone(),
            RuntimeHintSource {
                url: url.clone(),
                channel: keyspace.runtime_hint_channel(),
            },
            shutdown.clone(),
        )));
        state.limiter.mark_configured();
        plane.tasks.push(tokio::spawn(limiter_supervisor(
            state.limiter.clone(),
            url.clone(),
            keyspace.limits_namespace(),
            shutdown.clone(),
        )));

        if mode.serves_gateway() {
            load_limits_outage_policy(store, &state.limiter).await;
            plane.tasks.push(tokio::spawn(limits_policy_supervisor(
                store.clone(),
                state.limiter.clone(),
                shutdown.clone(),
            )));
            spawn_request_metadata_plane(&mut plane, state, args, store, url, &keyspace, shutdown);
        }
        if run_worker_in_process {
            plane.tasks.push(tokio::spawn(outbox_supervisor(
                store.clone(),
                url.clone(),
                keyspace.runtime_hint_channel(),
                shutdown.clone(),
            )));
            plane
                .tasks
                .push(tokio::spawn(request_metadata_consumer_supervisor(
                    store.clone(),
                    url.clone(),
                    keyspace.request_metadata_stream(),
                    request_metadata_consumer_name(),
                    keyspace.limits_namespace(),
                    shutdown.clone(),
                )));
            plane
                .tasks
                .push(tokio::spawn(cost_reconciliation_supervisor(
                    store.clone(),
                    url.clone(),
                    keyspace.limits_namespace(),
                    shutdown.clone(),
                )));
        }
    }
    if run_worker_in_process {
        plane.tasks.push(tokio::spawn(maintenance_supervisor(
            store.clone(),
            shutdown.clone(),
        )));
        plane
            .tasks
            .push(tokio::spawn(request_metadata_epoch_supervisor(
                store.clone(),
                shutdown.clone(),
            )));
    }
    Ok(plane)
}

fn spawn_request_metadata_plane(
    plane: &mut BackgroundPlane,
    state: &mut ProcessComposition,
    args: &ServeArgs,
    store: &Store,
    url: &str,
    keyspace: &Keyspace,
    shutdown: &watch::Receiver<bool>,
) {
    // Install the bounded local emitter even when Valkey is not up yet.
    // Its connection loop exposes retry/pending state and preserves events
    // until the configured bound is reached.
    let (emitter, receiver) = Emitter::bounded(8_192);
    state.request_metadata = Some(emitter.clone());
    let gateway_instance = format!(
        "{}:{}",
        std::env::var("HOSTNAME").unwrap_or_else(|_| "olp".to_owned()),
        args.listen_addr
    );
    plane
        .tasks
        .push(tokio::spawn(request_metadata_loss_reporter(
            store.clone(),
            emitter,
            gateway_instance,
            state.request_metadata_loss.clone(),
            shutdown.clone(),
        )));
    let request_metadata_writer_url = url.to_owned();
    let request_metadata_stream = keyspace.request_metadata_stream();
    let request_metadata_writer_shutdown = shutdown.clone();
    let (status_sender, status_receiver) = oneshot::channel();
    plane.request_metadata_writer_status = Some(status_receiver);
    plane.tasks.push(tokio::spawn(async move {
        let result: AppResult<()> = run_connecting(
            receiver,
            &request_metadata_writer_url,
            &request_metadata_stream,
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
