use std::{
    io::Write as _,
    path::{Path, PathBuf},
    sync::Arc,
    sync::atomic::{AtomicBool, AtomicUsize, Ordering},
    time::Duration,
};

use clap::Parser;
use olp_storage::{
    EncryptedTable, KeyHasher, KeyVersionReference, MasterKey, MasterKeyEncryptionStatus,
};
use tempfile::NamedTempFile;
use tokio::{sync::watch, task::JoinSet};

use super::{
    commands::stop_worker_tasks,
    config::{Cli, Command, MasterKeyAction, MasterKeyArgs},
    startup::{coordinate_shutdown, stop_background_tasks, wait_for_shutdown},
    validation::{
        check_secret_permissions, ensure_keyring_covers_references, load_bootstrap_token_digest,
    },
};

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
    assert!(
        hasher
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
