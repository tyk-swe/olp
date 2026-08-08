//! Process orchestration: per-run database, secret files, and the real `olp`
//! binary (`migrate` then `all`) against real PostgreSQL and Valkey.

use std::io::Read as _;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use base64::Engine as _;
use rand::RngCore as _;
use redis::{
    AsyncCommands as _,
    streams::{StreamReadOptions, StreamReadReply},
};
use sqlx::Connection as _;

pub struct Server {
    child: Child,
    stderr: Arc<Mutex<String>>,
    additional_children: Vec<Child>,
    additional_stderr: Vec<Arc<Mutex<String>>>,
    /// Public listener origin, e.g. `http://127.0.0.1:41234`.
    pub public_origin: String,
    /// Observability listener base.
    pub observability_base: String,
    /// Base64 setup token exactly as written to the bootstrap token file.
    pub setup_token: String,
    /// Connection URL for this run's database, so assertions about durable
    /// state can read the tables directly rather than through the API that
    /// wrote them.
    pub database_url: String,
    app_database_url: String,
    valkey_url: String,
    admin_url: String,
    database_name: String,
    run_dir: PathBuf,
    valkey_reservation: Option<ValkeyReservation>,
}

pub struct GatewayProcess {
    pub public_origin: String,
    pub observability_base: String,
}

#[derive(Clone, Copy)]
pub enum WorkerBoundary {
    RequestMetadata,
    RuntimeOutbox,
    None,
}

pub struct WorkerProcess {
    child_index: usize,
    start_marker: PathBuf,
    pub ownership_marker: PathBuf,
}

pub fn admin_url() -> String {
    std::env::var("OLP_E2E_DATABASE_ADMIN_URL")
        .unwrap_or_else(|_| "postgres://olp_test:olp_test@localhost:5433/postgres".to_owned())
}

/// The Valkey logical database leased by this test run.
///
/// Installation resources are durably namespaced, so multiple independently
/// migrated servers may safely share this exact URL. The lease protects the
/// run from unrelated parallel tests and owns whole-database cleanup only
/// after every sharing installation has stopped.
///
/// An explicit `OLP_E2E_VALKEY_URL` is honoured verbatim — CI gives the job a
/// Valkey service of its own. Otherwise the harness atomically reserves one of
/// logical databases 1–15 with a PostgreSQL session advisory lock and clears
/// the reserved database before use. PostgreSQL releases the lock if the test
/// process dies, and the next owner clears any state the abandoned run left.
async fn valkey(admin_url: &str) -> Result<(String, Option<ValkeyReservation>), String> {
    if let Ok(url) = std::env::var("OLP_E2E_VALKEY_URL") {
        return Ok((std::env::var("OLP_E2E_VALKEY_APP_URL").unwrap_or(url), None));
    }

    let mut byte = [0_u8; 1];
    rand::rng().fill_bytes(&mut byte);
    let start = usize::from(byte[0]) % 15;
    for offset in 0..15 {
        let database = u16::try_from(1 + (start + offset) % 15).unwrap();
        let mut lock = sqlx::postgres::PgConnection::connect(admin_url)
            .await
            .map_err(|error| format!("failed to connect for a Valkey reservation: {error}"))?;
        let acquired: bool = sqlx::query_scalar("SELECT pg_try_advisory_lock($1, $2)")
            .bind(0x4f4c_5002_i32)
            .bind(i32::from(database))
            .fetch_one(&mut lock)
            .await
            .map_err(|error| format!("failed to reserve a Valkey logical database: {error}"))?;
        if !acquired {
            lock.close().await.ok();
            continue;
        }
        if let Err(error) = flush_valkey(database).await {
            lock.close().await.ok();
            return Err(error);
        }
        let reservation = ValkeyReservation { database, lock };
        return Ok((reservation.url(), Some(reservation)));
    }
    Err("all 15 local Valkey logical databases are reserved by other E2E runs".to_owned())
}

struct ValkeyReservation {
    database: u16,
    lock: sqlx::postgres::PgConnection,
}

/// Owns one test Valkey lease independently of any OLP installation. Multiple
/// independently migrated servers can therefore share the exact URL while the
/// advisory lease remains held until every server has stopped.
pub struct SharedValkey {
    url: String,
    reservation: Option<ValkeyReservation>,
}

impl SharedValkey {
    pub async fn reserve() -> Result<Self, String> {
        let (url, reservation) = valkey(&admin_url()).await?;
        Ok(Self { url, reservation })
    }

    pub fn url(&self) -> &str {
        &self.url
    }

    pub async fn release(mut self) {
        if let Some(reservation) = self.reservation.take() {
            reservation.release().await;
        }
    }
}

impl ValkeyReservation {
    fn url(&self) -> String {
        format!("redis://localhost:6379/{}", self.database)
    }

    async fn release(self) {
        flush_valkey(self.database).await.ok();
        self.lock.close().await.ok();
    }
}

async fn flush_valkey(database: u16) -> Result<(), String> {
    let client = redis::Client::open(format!("redis://localhost:6379/{database}"))
        .map_err(|error| format!("invalid local Valkey URL: {error}"))?;
    let mut connection = client
        .get_multiplexed_async_connection()
        .await
        .map_err(|error| format!("failed to connect to local Valkey: {error}"))?;
    let _: () = redis::cmd("FLUSHDB")
        .query_async(&mut connection)
        .await
        .map_err(|error| format!("failed to clear local Valkey database {database}: {error}"))?;
    Ok(())
}

async fn seed_legacy_request_metadata_stream(valkey_url: &str) -> Result<String, String> {
    const LEGACY: &str = "olp:v2:request-metadata";
    const GROUP: &str = "olp:persistence";

    let client = redis::Client::open(valkey_url)
        .map_err(|error| format!("invalid shared Valkey URL: {error}"))?;
    let mut connection = client
        .get_multiplexed_async_connection()
        .await
        .map_err(|error| format!("failed to connect shared Valkey: {error}"))?;
    let _: i64 = redis::cmd("DEL")
        .arg(LEGACY)
        .query_async(&mut connection)
        .await
        .map_err(|error| format!("failed to clear legacy stream fixture: {error}"))?;
    let event_id: String = redis::cmd("XADD")
        .arg(LEGACY)
        .arg("*")
        .arg("event")
        .arg("legacy-pending-event")
        .query_async(&mut connection)
        .await
        .map_err(|error| format!("failed to seed legacy stream: {error}"))?;
    let _: String = redis::cmd("XGROUP")
        .arg("CREATE")
        .arg(LEGACY)
        .arg(GROUP)
        .arg("0")
        .query_async(&mut connection)
        .await
        .map_err(|error| format!("failed to seed legacy consumer group: {error}"))?;
    let delivered: StreamReadReply = connection
        .xread_options(
            &[LEGACY],
            &[">"],
            &StreamReadOptions::default().group(GROUP, "legacy-owner"),
        )
        .await
        .map_err(|error| format!("failed to establish legacy ownership: {error}"))?;
    if !delivered
        .keys
        .iter()
        .flat_map(|stream| &stream.ids)
        .any(|entry| entry.id == event_id)
    {
        return Err("legacy event was not delivered into the pending-entry list".to_owned());
    }
    Ok(event_id)
}

fn binary() -> Result<PathBuf, String> {
    let path = std::env::var("OLP_E2E_BIN")
        .map_err(|_| "OLP_E2E_BIN is unset; run via scripts/run-e2e-tests.sh".to_owned())?;
    let path = PathBuf::from(path);
    if !path.is_file() {
        return Err(format!("OLP_E2E_BIN does not exist: {}", path.display()));
    }
    Ok(path)
}

fn random_hex(bytes: usize) -> String {
    let mut buffer = vec![0_u8; bytes];
    rand::rng().fill_bytes(&mut buffer);
    buffer.iter().map(|byte| format!("{byte:02x}")).collect()
}

fn random_base64_secret() -> String {
    let mut buffer = [0_u8; 32];
    rand::rng().fill_bytes(&mut buffer);
    base64::engine::general_purpose::STANDARD.encode(buffer)
}

fn write_secret(path: &Path, contents: &str) -> Result<(), String> {
    std::fs::write(path, format!("{contents}\n"))
        .map_err(|error| format!("failed to write {}: {error}", path.display()))?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt as _;
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600))
            .map_err(|error| format!("failed to chmod {}: {error}", path.display()))?;
    }
    Ok(())
}

fn process_environment(
    run_dir: &Path,
    database: &str,
    valkey: &str,
    public_origin: String,
    observability_base: String,
    bootstrap: bool,
) -> Vec<(&'static str, String)> {
    let path = |name| run_dir.join(name).display().to_string();
    let mut environment = vec![
        ("OLP_DATABASE_URL", database.to_owned()),
        ("OLP_VALKEY_URL", valkey.to_owned()),
        ("OLP_LISTEN_ADDR", public_origin.replace("http://", "")),
        (
            "OLP_OBSERVABILITY_LISTEN_ADDR",
            observability_base.replace("http://", ""),
        ),
        ("OLP_PUBLIC_ORIGIN", public_origin),
        ("OLP_CONSOLE_DIR", path("console")),
        ("OLP_MEDIA_SPOOL_DIR", path("spool")),
        ("OLP_MASTER_KEY_FILE", path("master-key")),
        ("OLP_AUTH_HMAC_KEY_FILE", path("auth-hmac-key")),
        (
            "OLP_ALLOW_INSECURE_PROVIDER_ENDPOINTS_FOR_TESTS",
            "test-only".to_owned(),
        ),
        ("RUST_LOG", "olp=info".to_owned()),
    ];
    if bootstrap {
        environment.push(("OLP_BOOTSTRAP_TOKEN_FILE", path("bootstrap-token")));
    }
    environment
}

fn run_migrate(
    binary: &Path,
    environment: &[(&'static str, String)],
    database: &str,
    through_version: Option<i64>,
) -> Result<(), String> {
    let mut command = Command::new(binary);
    command
        .arg("migrate")
        .envs(environment.iter().map(|(key, value)| (*key, value.clone())))
        .env("OLP_DATABASE_URL", database);
    if let Some(target) = through_version {
        command
            .arg("--through-version")
            .arg(target.to_string())
            .env("OLP_ALLOW_PARTIAL_MIGRATIONS_FOR_TESTS", "test-only");
    }
    let migrate = command
        .output()
        .map_err(|error| format!("failed to run olp migrate: {error}"))?;
    if !migrate.status.success() {
        let stderr = String::from_utf8_lossy(&migrate.stderr);
        return Err(format!("olp migrate failed ({}): {stderr}", migrate.status));
    }
    Ok(())
}

fn free_port() -> Result<u16, String> {
    let listener = std::net::TcpListener::bind("127.0.0.1:0")
        .map_err(|error| format!("failed to bind an ephemeral port: {error}"))?;
    let port = listener
        .local_addr()
        .map_err(|error| format!("failed to read the ephemeral port: {error}"))?
        .port();
    drop(listener);
    Ok(port)
}

async fn create_database(admin: &str, name: &str) -> Result<(), String> {
    let mut connection = sqlx::postgres::PgConnection::connect(admin)
        .await
        .map_err(|error| format!("failed to connect to {admin}: {error}"))?;
    // Names are harness-generated hex, never user input.
    sqlx::query(sqlx::AssertSqlSafe(format!("CREATE DATABASE \"{name}\"")))
        .execute(&mut connection)
        .await
        .map_err(|error| format!("failed to create database {name}: {error}"))?;
    connection.close().await.ok();
    Ok(())
}

async fn drop_database(admin: &str, name: &str) {
    if let Ok(mut connection) = sqlx::postgres::PgConnection::connect(admin).await {
        sqlx::query(sqlx::AssertSqlSafe(format!(
            "DROP DATABASE IF EXISTS \"{name}\" WITH (FORCE)"
        )))
        .execute(&mut connection)
        .await
        .ok();
        connection.close().await.ok();
    }
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
    legacy_request_metadata_event_id: Option<String>,
}

#[derive(Clone, Copy)]
enum MigrationFixture {
    Current,
    LegacyRequestMetadataUpgrade,
}

/// Owns every resource acquired after `CREATE DATABASE` until startup either
/// succeeds or one centralized failure path releases all of it.
struct LaunchGuard {
    child: Option<Child>,
    stderr: Arc<Mutex<String>>,
    admin_url: String,
    database_name: String,
    run_dir: PathBuf,
    valkey_reservation: Option<ValkeyReservation>,
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
        process: &str,
        shared_valkey_url: Option<&str>,
        migration_fixture: MigrationFixture,
    ) -> Result<PreparedServer, String> {
        let console_dir = self.run_dir.join("console");
        let spool_dir = self.run_dir.join("spool");
        std::fs::create_dir_all(&console_dir)
            .and_then(|()| std::fs::create_dir_all(&spool_dir))
            .map_err(|error| format!("failed to create run directory: {error}"))?;

        let master_key_file = self.run_dir.join("master-key");
        let auth_hmac_key_file = self.run_dir.join("auth-hmac-key");
        let bootstrap_token_file = self.run_dir.join("bootstrap-token");
        let setup_token = random_base64_secret();
        write_secret(&master_key_file, &random_base64_secret())?;
        write_secret(&auth_hmac_key_file, &random_base64_secret())?;
        write_secret(&bootstrap_token_file, &setup_token)?;

        let (valkey_url, reservation) = match shared_valkey_url {
            Some(url) => (url.to_owned(), None),
            None => valkey(&self.admin_url).await?,
        };
        self.valkey_reservation = reservation;
        let mut legacy_request_metadata_event_id = None;

        for attempt in 1..=3 {
            let public_port = free_port()?;
            let mut observability_port = free_port()?;
            while observability_port == public_port {
                observability_port = free_port()?;
            }
            let public_origin = format!("http://127.0.0.1:{public_port}");
            let observability_base = format!("http://127.0.0.1:{observability_port}");
            let environment = process_environment(
                &self.run_dir,
                app_database,
                &valkey_url,
                public_origin.clone(),
                observability_base.clone(),
                true,
            );

            if attempt == 1 {
                match migration_fixture {
                    MigrationFixture::Current => {
                        run_migrate(binary, &environment, database, None)?;
                    }
                    MigrationFixture::LegacyRequestMetadataUpgrade => {
                        run_migrate(binary, &environment, database, Some(31))?;
                        legacy_request_metadata_event_id =
                            Some(seed_legacy_request_metadata_stream(&valkey_url).await?);
                        run_migrate(binary, &environment, database, None)?;
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
                capture_stderr(pipe, Arc::clone(&self.stderr));
            }
            self.child = Some(child);
            let pid = self.child.as_ref().expect("child was just installed").id();
            std::fs::write(self.run_dir.join("olp.pid"), format!("{pid}\n"))
                .map_err(|error| format!("failed to write olp pid file: {error}"))?;

            let startup = await_live(
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
                        legacy_request_metadata_event_id,
                    });
                }
                Err(error) => {
                    if let Some(mut child) = self.child.take() {
                        terminate_child(&mut child).await;
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
            terminate_child(&mut child).await;
        }
        if let Some(reservation) = self.valkey_reservation.take() {
            reservation.release().await;
        }
        drop_database(&self.admin_url, &self.database_name).await;
        std::fs::remove_dir_all(&self.run_dir).ok();
    }

    fn into_server(mut self, prepared: PreparedServer) -> Server {
        Server {
            child: self.child.take().expect("prepared server owns a child"),
            stderr: Arc::clone(&self.stderr),
            additional_children: Vec::new(),
            additional_stderr: Vec::new(),
            public_origin: prepared.public_origin,
            observability_base: prepared.observability_base,
            setup_token: prepared.setup_token,
            database_url: prepared.database_url,
            app_database_url: prepared.app_database_url,
            valkey_url: prepared.valkey_url,
            admin_url: self.admin_url,
            database_name: self.database_name,
            run_dir: self.run_dir,
            valkey_reservation: self.valkey_reservation.take(),
        }
    }
}

impl Server {
    pub async fn launch() -> Result<Self, String> {
        Self::launch_process("all", None).await
    }

    pub async fn launch_sharing_valkey(valkey_url: &str) -> Result<Self, String> {
        Self::launch_process("all", Some(valkey_url)).await
    }

    pub async fn launch_control() -> Result<Self, String> {
        Self::launch_process("control", None).await
    }

    pub async fn launch_control_sharing_valkey(valkey_url: &str) -> Result<Self, String> {
        Self::launch_process("control", Some(valkey_url)).await
    }

    pub async fn launch_control_from_legacy_request_metadata_upgrade(
        valkey_url: &str,
    ) -> Result<(Self, String), String> {
        let (server, event_id) = Self::launch_process_with_migration_fixture(
            "control",
            Some(valkey_url),
            MigrationFixture::LegacyRequestMetadataUpgrade,
        )
        .await?;
        let event_id = event_id.expect("legacy request metadata fixture seeds an event");
        Ok((server, event_id))
    }

    async fn launch_process(
        process: &str,
        shared_valkey_url: Option<&str>,
    ) -> Result<Self, String> {
        Self::launch_process_with_migration_fixture(
            process,
            shared_valkey_url,
            MigrationFixture::Current,
        )
        .await
        .map(|(server, _)| server)
    }

    async fn launch_process_with_migration_fixture(
        process: &str,
        shared_valkey_url: Option<&str>,
        migration_fixture: MigrationFixture,
    ) -> Result<(Self, Option<String>), String> {
        let binary = binary()?;
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
        let database_name = format!("olp_e2e_{run_token}_{}", random_hex(6));
        let database = database_url(&admin, &database_name)?;
        let app_database = match std::env::var("OLP_E2E_DATABASE_APP_ADMIN_URL") {
            Ok(app_admin) => database_url(&app_admin, &database_name)?,
            Err(_) => database.clone(),
        };
        create_database(&admin, &database_name).await?;

        let run_dir = std::env::temp_dir().join(format!("olp-e2e-{run_token}-{}", random_hex(6)));
        let mut guard = LaunchGuard::new(admin, database_name, run_dir);
        match guard
            .prepare(
                &binary,
                &database,
                &app_database,
                process,
                shared_valkey_url,
                migration_fixture,
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

    pub fn valkey_url(&self) -> &str {
        &self.valkey_url
    }

    pub fn stderr_tail(&self) -> String {
        let mut output = stderr_tail(&self.stderr);
        for (index, stderr) in self.additional_stderr.iter().enumerate() {
            output.push_str(&format!(
                "\n--- gateway {} ---\n{}",
                index + 2,
                stderr_tail(stderr)
            ));
        }
        output
    }

    pub async fn launch_gateway(&mut self) -> Result<GatewayProcess, String> {
        let binary = binary()?;
        for attempt in 1..=3 {
            let public_port = free_port()?;
            let mut observability_port = free_port()?;
            while observability_port == public_port {
                observability_port = free_port()?;
            }
            let public_origin = format!("http://127.0.0.1:{public_port}");
            let observability_base = format!("http://127.0.0.1:{observability_port}");
            let environment = process_environment(
                &self.run_dir,
                &self.app_database_url,
                &self.valkey_url,
                public_origin.clone(),
                observability_base.clone(),
                false,
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
                capture_stderr(pipe, Arc::clone(&stderr));
            }
            match await_live(&mut child, &stderr, &observability_base, "olp gateway").await {
                Ok(()) => {
                    let pid_file = self.run_dir.join(format!(
                        "gateway-{}.pid",
                        self.additional_children.len() + 2
                    ));
                    let pid = child.id();
                    self.additional_children.push(child);
                    self.additional_stderr.push(Arc::clone(&stderr));
                    std::fs::write(pid_file, format!("{pid}\n"))
                        .map_err(|error| format!("failed to write gateway pid file: {error}"))?;
                    return Ok(GatewayProcess {
                        public_origin,
                        observability_base,
                    });
                }
                Err(error) => {
                    terminate_child(&mut child).await;
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

    pub async fn launch_worker(
        &mut self,
        label: &str,
        boundary: WorkerBoundary,
    ) -> Result<WorkerProcess, String> {
        let binary = binary()?;
        let start_marker = self.run_dir.join(format!("{label}-start"));
        let ownership_marker = self.run_dir.join(format!("{label}-owned"));
        let environment = process_environment(
            &self.run_dir,
            &self.app_database_url,
            &self.valkey_url,
            "http://127.0.0.1:1".to_owned(),
            "http://127.0.0.1:2".to_owned(),
            false,
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
        if let Some(pipe) = child.stderr.take() {
            capture_stderr(pipe, Arc::clone(&stderr));
        }
        let child_index = self.additional_children.len();
        self.additional_children.push(child);
        self.additional_stderr.push(stderr);
        await_path_or_exit(
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

    pub fn release_worker(&self, worker: &WorkerProcess) -> Result<(), String> {
        let release = format!("{}.release", worker.start_marker.display());
        std::fs::write(release, b"release\n")
            .map_err(|error| format!("failed to release worker: {error}"))
    }

    pub async fn hard_kill_worker(&mut self, worker: &WorkerProcess) -> Result<(), String> {
        let child = &mut self.additional_children[worker.child_index];
        child
            .kill()
            .map_err(|error| format!("failed to SIGKILL worker: {error}"))?;
        child
            .wait()
            .map_err(|error| format!("failed to reap killed worker: {error}"))?;
        Ok(())
    }

    pub fn worker_ownership_marker(&self, label: &str) -> PathBuf {
        self.run_dir.join(format!("{label}-owned"))
    }

    /// SIGTERM, bounded wait, then SIGKILL as a last resort.
    pub async fn shutdown(mut self) -> String {
        for child in &mut self.additional_children {
            terminate_child(child).await;
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
            drop_database(&self.admin_url, &self.database_name).await;
            std::fs::remove_dir_all(&self.run_dir).ok();
        }
        self.stderr_tail()
    }

    async fn terminate(&mut self) {
        terminate_child(&mut self.child).await;
    }
}

async fn await_path_or_exit(child: &mut Child, path: &Path, process: &str) -> Result<(), String> {
    let deadline = Instant::now() + Duration::from_secs(30);
    loop {
        if path.exists() {
            return Ok(());
        }
        if let Some(status) = child
            .try_wait()
            .map_err(|error| format!("failed to inspect {process}: {error}"))?
        {
            return Err(format!(
                "{process} exited before reaching its start barrier: {status}"
            ));
        }
        if Instant::now() >= deadline {
            return Err(format!(
                "{process} did not reach its start barrier within 30s"
            ));
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
}

fn capture_stderr(pipe: impl std::io::Read + Send + 'static, sink: Arc<Mutex<String>>) {
    std::thread::spawn(move || {
        let mut reader = std::io::BufReader::new(pipe);
        let mut buffer = [0_u8; 4096];
        while let Ok(read) = reader.read(&mut buffer) {
            if read == 0 {
                break;
            }
            sink.lock()
                .unwrap()
                .push_str(&String::from_utf8_lossy(&buffer[..read]));
        }
    });
}

async fn await_live(
    child: &mut Child,
    stderr: &Arc<Mutex<String>>,
    observability_base: &str,
    process: &str,
) -> Result<(), String> {
    let client = reqwest::Client::builder()
        .timeout(Duration::from_secs(2))
        .build()
        .map_err(|error| format!("failed to build health-check client: {error}"))?;
    let url = format!("{observability_base}/health/live");
    let deadline = Instant::now() + Duration::from_secs(30);
    loop {
        if let Ok(Some(status)) = child.try_wait() {
            // Give the stderr reader a moment to drain the pipe so a released
            // port race can be recognized and retried.
            tokio::time::sleep(Duration::from_millis(50)).await;
            return Err(format!(
                "{process} exited during startup ({status}); stderr:\n{}",
                stderr_tail(stderr)
            ));
        }
        if let Ok(response) = client.get(&url).send().await
            && response.status().is_success()
        {
            return Ok(());
        }
        if Instant::now() > deadline {
            return Err(format!(
                "{process} did not become live within 30s; stderr:\n{}",
                stderr_tail(stderr)
            ));
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
}

fn stderr_tail(stderr: &Arc<Mutex<String>>) -> String {
    let stderr = stderr.lock().unwrap();
    let mut tail_start = stderr.len().saturating_sub(8_192);
    while !stderr.is_char_boundary(tail_start) {
        tail_start += 1;
    }
    stderr[tail_start..].to_owned()
}

async fn terminate_child(child: &mut Child) {
    match child.try_wait() {
        Ok(Some(_)) | Err(_) => return,
        Ok(None) => {}
    }
    let pid = child.id().to_string();
    Command::new("kill").args(["-TERM", &pid]).status().ok();
    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        match child.try_wait() {
            Ok(Some(_)) => break,
            Ok(None) if Instant::now() > deadline => {
                child.kill().ok();
                child.wait().ok();
                break;
            }
            Ok(None) => tokio::time::sleep(Duration::from_millis(100)).await,
            Err(_) => break,
        }
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
        for child in &mut self.additional_children {
            if matches!(child.try_wait(), Ok(None)) {
                child.kill().ok();
                child.wait().ok();
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
