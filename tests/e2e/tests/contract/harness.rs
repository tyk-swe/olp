//! Process orchestration: per-run database, secret files, and the real `olp`
//! binary (`migrate` then `all`) against real PostgreSQL and Valkey.

#[path = "harness/support.rs"]
mod support;
#[path = "harness/valkey.rs"]
mod valkey;

#[allow(unused_imports)]
pub(crate) use valkey::SharedValkey;

use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::{Arc, Mutex};

pub(crate) struct Server {
    child: Child,
    stderr: Arc<Mutex<String>>,
    additional_children: Vec<Child>,
    additional_stderr: Vec<Arc<Mutex<String>>>,
    additional_pid_files: Vec<PathBuf>,
    /// Public listener origin, e.g. `http://127.0.0.1:41234`.
    pub(crate) public_origin: String,
    /// Observability listener base.
    pub(crate) observability_base: String,
    /// Base64 setup token exactly as written to the bootstrap token file.
    pub(crate) setup_token: String,
    /// Connection URL for this run's database, so assertions about durable
    /// state can read the tables directly rather than through the API that
    /// wrote them.
    pub(crate) database_url: String,
    app_database_url: String,
    valkey_url: String,
    admin_url: String,
    database_name: String,
    run_dir: PathBuf,
    tracing_endpoint: Option<String>,
    valkey_reservation: Option<valkey::ValkeyReservation>,
}

pub(crate) struct GatewayProcess {
    pub(crate) public_origin: String,
    pub(crate) observability_base: String,
}

#[derive(Clone, Copy)]
pub(crate) enum WorkerBoundary {
    RequestMetadata,
    RuntimeOutbox,
    None,
}

pub(crate) struct WorkerProcess {
    child_index: usize,
    start_marker: PathBuf,
    pub(crate) ownership_marker: PathBuf,
}

pub(crate) fn admin_url() -> String {
    std::env::var("OLP_E2E_DATABASE_ADMIN_URL")
        .unwrap_or_else(|_| "postgres://olp_test:olp_test@localhost:5433/postgres".to_owned())
}

fn database_url(admin: &str, name: &str) -> Result<String, String> {
    let mut url = reqwest::Url::parse(admin)
        .map_err(|error| format!("invalid OLP_E2E_DATABASE_ADMIN_URL: {error}"))?;
    if url.cannot_be_a_base() || url.path().trim_matches('/').is_empty() {
        return Err(
            "OLP_E2E_DATABASE_ADMIN_URL must include a maintenance database path".to_owned(),
        );
    }
    url.set_path(&format!("/{name}"));
    Ok(url.into())
}

struct PreparedServer {
    public_origin: String,
    observability_base: String,
    setup_token: String,
    database_url: String,
    app_database_url: String,
    valkey_url: String,
    tracing_endpoint: Option<String>,
    legacy_request_metadata_event_id: Option<String>,
}

#[derive(Clone, Copy)]
enum MigrationFixture {
    Current,
    LegacyRequestMetadataUpgrade,
}

struct PrepareOptions<'a> {
    process: &'a str,
    shared_valkey_url: Option<&'a str>,
    tracing_endpoint: Option<&'a str>,
    migration_fixture: MigrationFixture,
}

/// Owns every resource acquired after `CREATE DATABASE` until startup either
/// succeeds or one centralized failure path releases all of it.
struct LaunchGuard {
    child: Option<Child>,
    stderr: Arc<Mutex<String>>,
    admin_url: String,
    database_name: String,
    run_dir: PathBuf,
    valkey_reservation: Option<valkey::ValkeyReservation>,
}

impl LaunchGuard {
    fn new(admin_url: String, database_name: String, run_dir: PathBuf) -> Self {
        Self {
            child: None,
            stderr: Arc::new(Mutex::new(String::new())),
            admin_url,
            database_name,
            run_dir,
            valkey_reservation: None,
        }
    }

    async fn prepare(
        &mut self,
        binary: &Path,
        database: &str,
        app_database: &str,
        options: PrepareOptions<'_>,
    ) -> Result<PreparedServer, String> {
        let PrepareOptions {
            process,
            shared_valkey_url,
            tracing_endpoint,
            migration_fixture,
        } = options;
        let console_dir = self.run_dir.join("console");
        let spool_dir = self.run_dir.join("spool");
        std::fs::create_dir_all(&console_dir)
            .and_then(|()| std::fs::create_dir_all(&spool_dir))
            .map_err(|error| format!("failed to create run directory: {error}"))?;

        let master_key_file = self.run_dir.join("master-key");
        let auth_hmac_key_file = self.run_dir.join("auth-hmac-key");
        let bootstrap_token_file = self.run_dir.join("bootstrap-token");
        let setup_token = support::random_base64_secret();
        support::write_secret(&master_key_file, &support::random_base64_secret())?;
        support::write_secret(&auth_hmac_key_file, &support::random_base64_secret())?;
        support::write_secret(&bootstrap_token_file, &setup_token)?;

        let (valkey_url, reservation) = match shared_valkey_url {
            Some(url) => (url.to_owned(), None),
            None => valkey::valkey(&self.admin_url).await?,
        };
        self.valkey_reservation = reservation;
        let mut legacy_request_metadata_event_id = None;

        for attempt in 1..=3 {
            let public_port = support::free_port()?;
            let mut observability_port = support::free_port()?;
            while observability_port == public_port {
                observability_port = support::free_port()?;
            }
            let public_origin = format!("http://127.0.0.1:{public_port}");
            let observability_base = format!("http://127.0.0.1:{observability_port}");
            let environment = support::process_environment(
                &self.run_dir,
                app_database,
                &valkey_url,
                public_origin.clone(),
                observability_base.clone(),
                true,
                tracing_endpoint,
            );

            if attempt == 1 {
                match migration_fixture {
                    MigrationFixture::Current => {
                        support::run_migrate(binary, &environment, database, None)?;
                    }
                    MigrationFixture::LegacyRequestMetadataUpgrade => {
                        support::run_migrate(binary, &environment, database, Some(31))?;
                        legacy_request_metadata_event_id =
                            Some(valkey::seed_legacy_request_metadata_stream(&valkey_url).await?);
                        support::run_migrate(binary, &environment, database, None)?;
                    }
                }
            }

            self.stderr = Arc::new(Mutex::new(String::new()));
            let mut child = Command::new(binary)
                .arg(process)
                .envs(environment.iter().map(|(key, value)| (*key, value.clone())))
                .stdout(Stdio::null())
                .stderr(Stdio::piped())
                .spawn()
                .map_err(|error| format!("failed to spawn olp all: {error}"))?;
            if let Some(pipe) = child.stderr.take() {
                support::capture_stderr(pipe, Arc::clone(&self.stderr));
            }
            self.child = Some(child);
            let pid = self.child.as_ref().expect("child was just installed").id();
            std::fs::write(self.run_dir.join("olp.pid"), format!("{pid}\n"))
                .map_err(|error| format!("failed to write olp pid file: {error}"))?;

            let startup = support::await_live(
                self.child.as_mut().expect("child was just installed"),
                &self.stderr,
                &observability_base,
                &format!("olp {process}"),
            )
            .await;
            match startup {
                Ok(()) => {
                    return Ok(PreparedServer {
                        public_origin,
                        observability_base,
                        setup_token,
                        database_url: database.to_owned(),
                        app_database_url: app_database.to_owned(),
                        valkey_url,
                        tracing_endpoint: tracing_endpoint.map(str::to_owned),
                        legacy_request_metadata_event_id,
                    });
                }
                Err(error) => {
                    if let Some(mut child) = self.child.take() {
                        support::terminate_child(&mut child).await;
                    }
                    std::fs::remove_file(self.run_dir.join("olp.pid")).ok();
                    let bind_race = error.contains("Address already in use")
                        || error.contains("os error 48")
                        || error.contains("os error 98")
                        || error.contains("os error 10048");
                    if bind_race && attempt < 3 {
                        continue;
                    }
                    return Err(error);
                }
            }
        }
        unreachable!("bounded startup loop either succeeds or returns an error")
    }

    async fn cleanup(&mut self) {
        if let Some(mut child) = self.child.take() {
            support::terminate_child(&mut child).await;
        }
        if let Some(reservation) = self.valkey_reservation.take() {
            reservation.release().await;
        }
        support::drop_database(&self.admin_url, &self.database_name).await;
        std::fs::remove_dir_all(&self.run_dir).ok();
    }

    fn into_server(mut self, prepared: PreparedServer) -> Server {
        Server {
            child: self.child.take().expect("prepared server owns a child"),
            stderr: Arc::clone(&self.stderr),
            additional_children: Vec::new(),
            additional_stderr: Vec::new(),
            additional_pid_files: Vec::new(),
            public_origin: prepared.public_origin,
            observability_base: prepared.observability_base,
            setup_token: prepared.setup_token,
            database_url: prepared.database_url,
            app_database_url: prepared.app_database_url,
            valkey_url: prepared.valkey_url,
            admin_url: self.admin_url,
            database_name: self.database_name,
            run_dir: self.run_dir,
            tracing_endpoint: prepared.tracing_endpoint,
            valkey_reservation: self.valkey_reservation.take(),
        }
    }
}

impl Server {
    pub(crate) async fn launch() -> Result<Self, String> {
        Self::launch_process("all", None, None).await
    }

    pub(crate) async fn launch_traced(tracing_endpoint: &str) -> Result<Self, String> {
        Self::launch_process("all", None, Some(tracing_endpoint)).await
    }

    pub(crate) async fn launch_sharing_valkey(valkey_url: &str) -> Result<Self, String> {
        Self::launch_process("all", Some(valkey_url), None).await
    }

    pub(crate) async fn launch_control() -> Result<Self, String> {
        Self::launch_process("control", None, None).await
    }

    pub(crate) async fn launch_control_sharing_valkey(valkey_url: &str) -> Result<Self, String> {
        Self::launch_process("control", Some(valkey_url), None).await
    }

    pub(crate) async fn launch_control_from_legacy_request_metadata_upgrade(
        valkey_url: &str,
    ) -> Result<(Self, String), String> {
        let (server, event_id) = Self::launch_process_with_migration_fixture(
            "control",
            Some(valkey_url),
            None,
            MigrationFixture::LegacyRequestMetadataUpgrade,
        )
        .await?;
        let event_id = event_id.expect("legacy request metadata fixture seeds an event");
        Ok((server, event_id))
    }

    async fn launch_process(
        process: &str,
        shared_valkey_url: Option<&str>,
        tracing_endpoint: Option<&str>,
    ) -> Result<Self, String> {
        Self::launch_process_with_migration_fixture(
            process,
            shared_valkey_url,
            tracing_endpoint,
            MigrationFixture::Current,
        )
        .await
        .map(|(server, _)| server)
    }

    async fn launch_process_with_migration_fixture(
        process: &str,
        shared_valkey_url: Option<&str>,
        tracing_endpoint: Option<&str>,
        migration_fixture: MigrationFixture,
    ) -> Result<(Self, Option<String>), String> {
        let binary = support::binary()?;
        let run_token = std::env::var("OLP_E2E_RUN_TOKEN").map_err(|_| {
            "OLP_E2E_RUN_TOKEN is unset; run via scripts/run-e2e-tests.sh".to_owned()
        })?;
        if run_token.len() != 10
            || !run_token
                .bytes()
                .all(|byte| byte.is_ascii_hexdigit() && !byte.is_ascii_uppercase())
        {
            return Err("OLP_E2E_RUN_TOKEN must be 10 lowercase hexadecimal digits".to_owned());
        }

        let admin = admin_url();
        let database_name = format!("olp_e2e_{run_token}_{}", support::random_hex(6));
        let database = database_url(&admin, &database_name)?;
        let app_database = match std::env::var("OLP_E2E_DATABASE_APP_ADMIN_URL") {
            Ok(app_admin) => database_url(&app_admin, &database_name)?,
            Err(_) => database.clone(),
        };
        support::create_database(&admin, &database_name).await?;

        let run_dir =
            std::env::temp_dir().join(format!("olp-e2e-{run_token}-{}", support::random_hex(6)));
        let mut guard = LaunchGuard::new(admin, database_name, run_dir);
        match guard
            .prepare(
                &binary,
                &database,
                &app_database,
                PrepareOptions {
                    process,
                    shared_valkey_url,
                    tracing_endpoint,
                    migration_fixture,
                },
            )
            .await
        {
            Ok(prepared) => {
                let legacy_request_metadata_event_id =
                    prepared.legacy_request_metadata_event_id.clone();
                Ok((
                    guard.into_server(prepared),
                    legacy_request_metadata_event_id,
                ))
            }
            Err(error) => {
                guard.cleanup().await;
                Err(error)
            }
        }
    }

    pub(crate) fn valkey_url(&self) -> &str {
        &self.valkey_url
    }

    pub(crate) fn stderr_tail(&self) -> String {
        let mut output = support::stderr_tail(&self.stderr);
        for (index, stderr) in self.additional_stderr.iter().enumerate() {
            output.push_str(&format!(
                "\n--- gateway {} ---\n{}",
                index + 2,
                support::stderr_tail(stderr)
            ));
        }
        output
    }

    pub(crate) async fn launch_gateway(&mut self) -> Result<GatewayProcess, String> {
        let binary = support::binary()?;
        for attempt in 1..=3 {
            let public_port = support::free_port()?;
            let mut observability_port = support::free_port()?;
            while observability_port == public_port {
                observability_port = support::free_port()?;
            }
            let public_origin = format!("http://127.0.0.1:{public_port}");
            let observability_base = format!("http://127.0.0.1:{observability_port}");
            let environment = support::process_environment(
                &self.run_dir,
                &self.app_database_url,
                &self.valkey_url,
                public_origin.clone(),
                observability_base.clone(),
                false,
                self.tracing_endpoint.as_deref(),
            );
            let stderr = Arc::new(Mutex::new(String::new()));
            let mut child = Command::new(&binary)
                .arg("gateway")
                .envs(environment)
                .stdout(Stdio::null())
                .stderr(Stdio::piped())
                .spawn()
                .map_err(|error| format!("failed to spawn olp gateway: {error}"))?;
            if let Some(pipe) = child.stderr.take() {
                support::capture_stderr(pipe, Arc::clone(&stderr));
            }
            match support::await_live(&mut child, &stderr, &observability_base, "olp gateway").await
            {
                Ok(()) => {
                    let pid_file = self.run_dir.join(format!(
                        "gateway-{}.pid",
                        self.additional_children.len() + 2
                    ));
                    let pid = child.id();
                    if let Err(error) = std::fs::write(&pid_file, format!("{pid}\n")) {
                        support::terminate_child(&mut child).await;
                        return Err(format!("failed to write gateway pid file: {error}"));
                    }
                    self.additional_children.push(child);
                    self.additional_stderr.push(Arc::clone(&stderr));
                    self.additional_pid_files.push(pid_file);
                    return Ok(GatewayProcess {
                        public_origin,
                        observability_base,
                    });
                }
                Err(error) => {
                    support::terminate_child(&mut child).await;
                    let bind_race = error.contains("Address already in use")
                        || error.contains("os error 48")
                        || error.contains("os error 98")
                        || error.contains("os error 10048");
                    if !bind_race || attempt == 3 {
                        return Err(error);
                    }
                }
            }
        }
        unreachable!("bounded startup loop either succeeds or returns an error")
    }

    pub(crate) async fn launch_worker(
        &mut self,
        label: &str,
        boundary: WorkerBoundary,
    ) -> Result<WorkerProcess, String> {
        let binary = support::binary()?;
        let start_marker = self.run_dir.join(format!("{label}-start"));
        let ownership_marker = self.run_dir.join(format!("{label}-owned"));
        let environment = support::process_environment(
            &self.run_dir,
            &self.app_database_url,
            &self.valkey_url,
            "http://127.0.0.1:1".to_owned(),
            "http://127.0.0.1:2".to_owned(),
            false,
            None,
        );
        let stderr = Arc::new(Mutex::new(String::new()));
        let mut command = Command::new(binary);
        command
            .arg("worker")
            .envs(environment)
            .env("OLP_TEST_WORKER_START_MARKER", &start_marker)
            .stdout(Stdio::null())
            .stderr(Stdio::piped());
        match boundary {
            WorkerBoundary::RequestMetadata => {
                command.env("OLP_TEST_REQUEST_METADATA_OWNED_MARKER", &ownership_marker);
            }
            WorkerBoundary::RuntimeOutbox => {
                command.env("OLP_TEST_OUTBOX_OWNED_MARKER", &ownership_marker);
            }
            WorkerBoundary::None => {}
        }
        let mut child = command
            .spawn()
            .map_err(|error| format!("failed to spawn {label}: {error}"))?;
        let pid_file = self.run_dir.join(format!("{label}.pid"));
        let pid = child.id();
        if let Err(error) = std::fs::write(&pid_file, format!("{pid}\n")) {
            support::terminate_child(&mut child).await;
            return Err(format!("failed to write {label} pid file: {error}"));
        }
        if let Some(pipe) = child.stderr.take() {
            support::capture_stderr(pipe, Arc::clone(&stderr));
        }
        let child_index = self.additional_children.len();
        self.additional_children.push(child);
        self.additional_stderr.push(stderr);
        self.additional_pid_files.push(pid_file);
        support::await_path_or_exit(
            &mut self.additional_children[child_index],
            &start_marker,
            label,
        )
        .await?;
        Ok(WorkerProcess {
            child_index,
            start_marker,
            ownership_marker,
        })
    }

    pub(crate) fn release_worker(&self, worker: &WorkerProcess) -> Result<(), String> {
        let release = format!("{}.release", worker.start_marker.display());
        std::fs::write(release, b"release\n")
            .map_err(|error| format!("failed to release worker: {error}"))
    }

    pub(crate) async fn hard_kill_worker(&mut self, worker: &WorkerProcess) -> Result<(), String> {
        let child = &mut self.additional_children[worker.child_index];
        child
            .kill()
            .map_err(|error| format!("failed to SIGKILL worker: {error}"))?;
        child
            .wait()
            .map_err(|error| format!("failed to reap killed worker: {error}"))?;
        if let Some(pid_file) = self.additional_pid_files.get(worker.child_index) {
            std::fs::remove_file(pid_file).ok();
        }
        Ok(())
    }

    pub(crate) fn worker_ownership_marker(&self, label: &str) -> PathBuf {
        self.run_dir.join(format!("{label}-owned"))
    }

    /// SIGTERM, bounded wait, then SIGKILL as a last resort.
    pub(crate) async fn shutdown(mut self) -> String {
        for (index, child) in self.additional_children.iter_mut().enumerate() {
            support::terminate_child(child).await;
            if let Some(pid_file) = self.additional_pid_files.get(index) {
                std::fs::remove_file(pid_file).ok();
            }
        }
        self.terminate().await;
        if let Some(reservation) = self.valkey_reservation.take() {
            reservation.release().await;
        }
        if std::env::var("OLP_E2E_KEEP_DB").as_deref() == Ok("1") {
            eprintln!(
                "OLP_E2E_KEEP_DB=1: keeping database {} and run dir {}",
                self.database_name,
                self.run_dir.display()
            );
        } else {
            support::drop_database(&self.admin_url, &self.database_name).await;
            std::fs::remove_dir_all(&self.run_dir).ok();
        }
        self.stderr_tail()
    }

    async fn terminate(&mut self) {
        support::terminate_child(&mut self.child).await;
    }
}

/// Last-resort cleanup when an ordinarily owned `Server` is dropped.
///
/// The suite's shared `Server` lives in a static and therefore has no process
/// exit destructor. `scripts/run-e2e-tests.sh` owns that case with its
/// run-scoped process/database trap, including panics, filters that exclude the
/// graceful teardown test, and interruptions.
impl Drop for Server {
    fn drop(&mut self) {
        for (index, child) in self.additional_children.iter_mut().enumerate() {
            if matches!(child.try_wait(), Ok(None)) {
                child.kill().ok();
                child.wait().ok();
            }
            if let Some(pid_file) = self.additional_pid_files.get(index) {
                std::fs::remove_file(pid_file).ok();
            }
        }
        if matches!(self.child.try_wait(), Ok(None)) {
            self.child.kill().ok();
            self.child.wait().ok();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::database_url;

    #[test]
    fn database_url_replaces_only_the_database_path() {
        let actual = database_url(
            "postgres://user:pass@db.example:5432/postgres?sslmode=require&application_name=e2e",
            "olp_e2e_abc",
        )
        .expect("valid database URL");
        assert_eq!(
            actual,
            "postgres://user:pass@db.example:5432/olp_e2e_abc?sslmode=require&application_name=e2e"
        );
    }

    #[test]
    fn database_url_rejects_malformed_or_pathless_admin_urls() {
        assert!(database_url("not a URL", "olp_e2e_abc").is_err());
        assert!(database_url("postgres://db.example", "olp_e2e_abc").is_err());
    }
}
