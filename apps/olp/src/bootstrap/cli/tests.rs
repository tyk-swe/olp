use std::{
    io::Write as _,
    path::{Path, PathBuf},
    sync::Arc,
    sync::atomic::{AtomicBool, AtomicUsize, Ordering},
    time::Duration,
};

use clap::Parser;
use olp_db::{
    security::envelope::MasterKey, security::key_material::AuthHmacKey,
    security::rotation::EncryptedTable, security::rotation::KeyVersionReference,
    security::rotation::MasterKeyEncryptionStatus,
};
use olp_engine::providers::EgressPolicy;

use crate::bootstrap::state::BodyLimits;
use tempfile::NamedTempFile;
use tokio::{sync::watch, task::JoinSet};

use super::{
    config::{Cli, Command, MasterKeyAction, MasterKeyArgs},
    lifecycle::{
        coordinate_shutdown, resolve_request_metadata_writer_error, shutdown_reason,
        stop_background_tasks, wait_for_shutdown,
    },
    migrate::legacy_request_metadata_stream_claim_token,
    validation::{
        check_secret_permissions, ensure_keyring_covers_references, load_bootstrap_token_digest,
    },
    worker::{request_metadata_consumer_name_from, stop_worker_tasks},
};

#[test]
fn request_metadata_consumer_names_are_process_unique_and_bounded() {
    let first_epoch = uuid::Uuid::parse_str("018f0000-0000-7000-8000-000000000001").unwrap();
    let second_epoch = uuid::Uuid::parse_str("018f0000-0000-7000-8000-000000000002").unwrap();
    let first = request_metadata_consumer_name_from("Worker Pod/One", 42, first_epoch);
    let second = request_metadata_consumer_name_from("Worker Pod/One", 42, second_epoch);

    assert_eq!(first, "worker-pod-one-42-018f0000000070008000000000000001");
    assert_ne!(first, second);
    assert!(
        request_metadata_consumer_name_from(&"x".repeat(200), u32::MAX, first_epoch).len() <= 92
    );
}

#[test]
fn legacy_request_metadata_stream_claim_token_ignores_credentials_and_non_identity_query() {
    let first = legacy_request_metadata_stream_claim_token(
        "postgres://user:secret@db.example:5432/olp?sslmode=require&application_name=one&user=one&password=secret",
    )
    .unwrap();
    let second = legacy_request_metadata_stream_claim_token(
        "postgres://other:rotated@db.example:5432/olp?sslmode=disable&application_name=two&user=two&password=rotated",
    )
    .unwrap();
    let other_database =
        legacy_request_metadata_stream_claim_token("postgres://user:secret@db.example:5432/other")
            .unwrap();

    assert_eq!(first, second);
    assert_ne!(first, other_database);
    assert!(first.starts_with("database-url-sha256-v1:"));
    assert!(!first.contains("secret"));
}

#[test]
fn legacy_request_metadata_stream_claim_token_preserves_query_database_identity() {
    let cases = [
        (
            "postgres://u:p@placeholder:5432/olp?host=db-one.example&sslmode=require",
            "postgres://u:p@placeholder:5432/olp?host=db-two.example&sslmode=require",
        ),
        (
            "postgres://u:p@placeholder:5432/olp?hostaddr=10.0.0.1",
            "postgres://u:p@placeholder:5432/olp?hostaddr=10.0.0.2",
        ),
        (
            "postgres://u:p@db.example:5432/olp?port=5432&application_name=one",
            "postgres://u:p@db.example:5432/olp?port=6432&application_name=one",
        ),
        (
            "postgres://u:p@db.example:5432/postgres?dbname=tenant_one&sslmode=require",
            "postgres://u:p@db.example:5432/postgres?dbname=tenant_two&sslmode=require",
        ),
    ];

    for (first, second) in cases {
        assert_ne!(
            legacy_request_metadata_stream_claim_token(first).unwrap(),
            legacy_request_metadata_stream_claim_token(second).unwrap(),
            "identity parameters must affect the claim token: {first}"
        );
    }
}

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

#[test]
fn health_probe_cli_is_shell_free_and_requires_no_configuration() {
    let cli = Cli::try_parse_from(["olp", "health-probe"]).unwrap();
    assert!(matches!(cli.command, Command::HealthProbe));
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
    let auth_hmac_key = AuthHmacKey::new([9; 32]);
    let digest = load_bootstrap_token_digest(token.path(), &auth_hmac_key)
        .await
        .unwrap();
    assert!(
        auth_hmac_key
            .verify_bootstrap_token_digest("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", &digest)
    );
}

#[test]
fn server_cli_parses_bootstrap_and_trusted_proxy_configuration() {
    let cli = Cli::try_parse_from([
        "olp",
        "control",
        "--database-url",
        "postgres://example/olp",
        "--auth-hmac-key-file",
        "/run/secrets/auth-hmac-key",
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
    assert_eq!(args.http_max_in_flight_inference_requests, 256);
    assert_eq!(args.http_max_in_flight_management_requests, 32);
}

#[test]
fn server_cli_accepts_an_empty_trusted_proxy_list() {
    let cli = Cli::try_parse_from([
        "olp",
        "control",
        "--database-url",
        "postgres://example/olp",
        "--trusted-proxy-cidrs",
        "",
    ])
    .unwrap();
    let Command::Control(args) = cli.command else {
        panic!("expected control command");
    };
    assert!(args.trusted_proxy_cidrs.is_empty());
}

#[test]
fn server_cli_rejects_invalid_admission_capacities() {
    for (flag, value) in [
        ("--http-max-in-flight-inference-requests", "0"),
        ("--http-max-in-flight-management-requests", "1000001"),
    ] {
        let result = Cli::try_parse_from([
            "olp",
            "control",
            "--database-url",
            "postgres://example/olp",
            flag,
            value,
        ]);
        assert!(result.is_err(), "{flag}={value} must be rejected");
    }
}

#[test]
fn server_cli_bounds_connection_age_and_drain_timeout() {
    for (flag, value) in [
        ("--http-connection-max-age-seconds", "0"),
        ("--http-connection-max-age-seconds", "86401"),
        ("--http-connection-drain-timeout-seconds", "0"),
        ("--http-connection-drain-timeout-seconds", "3601"),
    ] {
        let result = Cli::try_parse_from([
            "olp",
            "control",
            "--database-url",
            "postgres://example/olp",
            flag,
            value,
        ]);
        assert!(result.is_err(), "{flag}={value} must be rejected");
    }
    let cli = Cli::try_parse_from([
        "olp",
        "control",
        "--database-url",
        "postgres://example/olp",
        "--http-connection-max-age-seconds",
        "600",
        "--http-connection-drain-timeout-seconds",
        "45",
    ])
    .unwrap();
    let Command::Control(args) = cli.command else {
        panic!("expected control command");
    };
    assert_eq!(args.http_connection_max_age_seconds, 600);
    assert_eq!(args.http_connection_drain_timeout_seconds, 45);
}

#[test]
fn server_cli_bounds_body_and_provider_response_caps() {
    let mib = 1024 * 1024;
    for (flag, value) in [
        ("--http-max-json-body-bytes", 64 * 1024 - 1),
        ("--http-max-json-body-bytes", 64 * mib + 1),
        ("--http-max-media-body-bytes", mib - 1),
        ("--http-max-media-body-bytes", 1024 * mib + 1),
        ("--http-max-inline-media-items", 0),
        ("--http-max-inline-media-items", 65),
        ("--http-max-inline-media-item-bytes", 1023),
        ("--http-max-inline-media-total-bytes", 64 * mib + 1),
        ("--provider-max-response-bytes", mib - 1),
        ("--provider-max-response-bytes", 256 * mib + 1),
        ("--provider-max-event-bytes", 64 * 1024 - 1),
        ("--provider-max-event-bytes", 256 * mib + 1),
    ] {
        let value = value.to_string();
        let result = Cli::try_parse_from([
            "olp",
            "control",
            "--database-url",
            "postgres://example/olp",
            flag,
            &value,
        ]);
        assert!(result.is_err(), "{flag}={value} must be rejected");
    }
    let cli = Cli::try_parse_from([
        "olp",
        "control",
        "--database-url",
        "postgres://example/olp",
        "--http-max-json-body-bytes",
        "8388608",
        "--http-max-media-body-bytes",
        "134217728",
        "--http-max-inline-media-items",
        "8",
        "--http-max-inline-media-item-bytes",
        "2097152",
        "--http-max-inline-media-total-bytes",
        "4194304",
        "--provider-max-response-bytes",
        "33554432",
        "--provider-max-event-bytes",
        "2097152",
    ])
    .unwrap();
    let Command::Control(args) = cli.command else {
        panic!("expected control command");
    };
    let limits = args.body_limits.limits();
    assert_eq!(limits.json_body_bytes, 8 * mib);
    assert_eq!(limits.media_body_bytes, 128 * mib);
    assert_eq!(limits.inline_media_items, 8);
    assert_eq!(limits.inline_media_item_bytes, 2 * mib);
    assert_eq!(limits.inline_media_total_bytes, 4 * mib);
    let response_limits = args.provider_response_limits.limits().unwrap();
    assert_eq!(response_limits.max_response_bytes, 32 * mib);
    assert_eq!(response_limits.max_event_bytes, 2 * mib);

    let defaults = Cli::try_parse_from([
        "olp",
        "control",
        "--database-url",
        "postgres://example/olp",
        "--provider-max-event-bytes",
        "33554432",
    ])
    .unwrap();
    let Command::Control(args) = defaults.command else {
        panic!("expected control command");
    };
    assert_eq!(args.body_limits.limits(), BodyLimits::default());
    assert!(args.provider_response_limits.limits().is_err());
}

#[test]
fn server_cli_parses_provider_egress_allowlists() {
    let cli = Cli::try_parse_from([
        "olp",
        "control",
        "--database-url",
        "postgres://example/olp",
        "--provider-egress-allow-cidrs",
        "10.0.0.0/8, ::1/128",
        "--provider-egress-allow-http-hosts",
        "vllm.internal,127.0.0.1,[::1]",
    ])
    .unwrap();
    let Command::Control(args) = cli.command else {
        panic!("expected control command");
    };
    let policy = args.provider_egress.policy();
    assert_eq!(policy.allowed_networks().len(), 2);
    assert_eq!(
        policy.plain_http_hosts(),
        ["vllm.internal", "127.0.0.1", "::1"]
    );
    assert!(policy.permits_address("10.1.2.3".parse().unwrap()));
    assert!(!policy.permits_address("192.168.0.1".parse().unwrap()));
    assert!(policy.permits_plain_http("vllm.internal"));
    assert!(policy.permits_plain_http("[::1]"));
    assert!(!policy.permits_plain_http("other.internal"));

    let cli = Cli::try_parse_from([
        "olp",
        "doctor",
        "--database-url",
        "postgres://example/olp",
        "--valkey-url",
        "redis://example/0",
        "--master-key-file",
        "/run/secrets/master-key",
        "--auth-hmac-key-file",
        "/run/secrets/auth-hmac-key",
        "--provider-egress-allow-cidrs",
        "",
        "--provider-egress-allow-http-hosts",
        "",
    ])
    .unwrap();
    let Command::Doctor(args) = cli.command else {
        panic!("expected doctor command");
    };
    assert_eq!(args.provider_egress.policy(), EgressPolicy::default());
}

#[test]
fn server_cli_parses_tracing_settings_and_rejects_invalid_ratios() {
    let cli = Cli::try_parse_from([
        "olp",
        "gateway",
        "--database-url",
        "postgres://example/olp",
        "--otlp-traces-endpoint",
        "http://collector:4318/v1/traces",
        "--otlp-headers-file",
        "/run/secrets/otlp-headers",
        "--trace-sample-ratio",
        "0.25",
        "--trace-propagate-upstream=false",
        "--trace-accept-inbound=false",
    ])
    .unwrap();
    let Command::Gateway(args) = cli.command else {
        panic!("expected gateway command");
    };
    assert_eq!(
        args.tracing.otlp_traces_endpoint.as_deref(),
        Some("http://collector:4318/v1/traces")
    );
    assert_eq!(
        args.tracing.otlp_headers_file.as_deref(),
        Some(std::path::Path::new("/run/secrets/otlp-headers"))
    );
    assert_eq!(args.tracing.trace_sample_ratio, 0.25);
    assert!(!args.tracing.trace_propagate_upstream);
    assert!(!args.tracing.trace_accept_inbound);

    for ratio in ["-0.1", "1.1", "NaN", "inf"] {
        assert!(
            Cli::try_parse_from([
                "olp",
                "gateway",
                "--database-url",
                "postgres://example/olp",
                "--trace-sample-ratio",
                ratio,
            ])
            .is_err(),
            "ratio {ratio} must be rejected"
        );
    }
}

#[test]
fn server_cli_rejects_malformed_provider_egress_allowlists() {
    for (flag, value) in [
        ("--provider-egress-allow-cidrs", "10.0.0.0"),
        ("--provider-egress-allow-cidrs", "10.0.0.0/33"),
        ("--provider-egress-allow-cidrs", "vllm.internal"),
        ("--provider-egress-allow-http-hosts", "VLLM.internal"),
        ("--provider-egress-allow-http-hosts", "http://vllm.internal"),
        ("--provider-egress-allow-http-hosts", "vllm.internal:8000"),
        ("--provider-egress-allow-http-hosts", "-bad.host"),
        ("--provider-egress-allow-http-hosts", "bad_host"),
    ] {
        let result = Cli::try_parse_from([
            "olp",
            "control",
            "--database-url",
            "postgres://example/olp",
            flag,
            value,
        ]);
        assert!(result.is_err(), "{flag}={value} must be rejected");
    }
}

#[test]
fn migration_cli_parses_valkey_preflight_dependency() {
    let cli = Cli::try_parse_from([
        "olp",
        "migrate",
        "--database-url",
        "postgres://example/olp",
        "--valkey-url",
        "redis://valkey:6379",
    ])
    .unwrap();
    let Command::Migrate(args) = cli.command else {
        panic!("expected migrate command");
    };
    assert_eq!(args.persistence.valkey_url, "redis://valkey:6379");
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
async fn request_metadata_writer_failure_stops_listeners_and_surfaces_error() {
    let (listener_shutdown, listener_receiver) = watch::channel(false);
    let (background_shutdown, background_receiver) = watch::channel(false);
    let (shutdown_sender, shutdown_receiver) = tokio::sync::oneshot::channel();
    let (status_sender, status_receiver) = tokio::sync::oneshot::channel();
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
    let reporter = tokio::spawn(async move {
        shutdown_sender.send(()).unwrap();
        drain_started_receiver.await.unwrap();
        status_sender
            .send(Err(
                std::io::Error::other("legacy stream is not drained").into()
            ))
            .unwrap();
        release_drain.send(true).unwrap();
    });
    let mut status_receiver = status_receiver;
    let (_, _, terminal_error) = coordinate_shutdown(
        public_server,
        observability_server,
        shutdown_reason(
            async {
                let _ = shutdown_receiver.await;
            },
            Some(&mut status_receiver),
        ),
        listener_shutdown,
        background_shutdown,
    )
    .await;
    reporter.await.unwrap();
    let error = resolve_request_metadata_writer_error(Some(status_receiver), terminal_error).await;

    assert_eq!(error.unwrap().to_string(), "legacy stream is not drained");
    assert!(*background_receiver.borrow());
}

#[tokio::test]
async fn request_metadata_writer_failure_wins_when_shutdown_is_also_ready() {
    let (status_sender, mut status_receiver) = tokio::sync::oneshot::channel();
    status_sender
        .send(Err(std::io::Error::other("writer failed").into()))
        .unwrap();

    let error = shutdown_reason(async {}, Some(&mut status_receiver))
        .await
        .unwrap();

    assert_eq!(error.to_string(), "writer failed");
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
