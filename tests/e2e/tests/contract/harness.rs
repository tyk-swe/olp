//! Process orchestration: per-run database, secret files, and the real `olp`
//! binary (`migrate` then `all`) against real PostgreSQL and Valkey.

use std::io::Read as _;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use base64::Engine as _;
use rand::RngCore as _;
use sqlx::Connection as _;
use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};
use tokio::net::TcpStream;

pub struct Server {
    child: Child,
    stderr: Arc<Mutex<String>>,
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
    admin_url: String,
    database_name: String,
    run_dir: PathBuf,
    valkey_reservation: Option<ValkeyReservation>,
}

pub fn admin_url() -> String {
    std::env::var("OLP_E2E_DATABASE_ADMIN_URL")
        .unwrap_or_else(|_| "postgres://olp_test:olp_test@localhost:5433/postgres".to_owned())
}

/// The Valkey this run may use exclusively.
///
/// `docs/operations.md` requires each installation to have "an isolated
/// database and a fresh isolated Valkey", because the request-metadata stream
/// key is a fixed global name: any other worker attached to the same keyspace
/// consumes and acknowledges this installation's envelopes, and its telemetry
/// simply never arrives. Sharing one Valkey therefore does not degrade the
/// suite, it silently empties the request log.
///
/// An explicit `OLP_E2E_VALKEY_URL` is honoured verbatim — CI gives the job a
/// Valkey service of its own. Otherwise the harness atomically reserves one of
/// logical databases 1–15 with a PostgreSQL session advisory lock and clears
/// the reserved database before use. PostgreSQL releases the lock if the test
/// process dies, and the next owner clears any state the abandoned run left.
async fn valkey(admin_url: &str) -> Result<(String, Option<ValkeyReservation>), String> {
    if let Ok(url) = std::env::var("OLP_E2E_VALKEY_URL") {
        return Ok((url, None));
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
        if let Err(error) = valkey_command(database, &["FLUSHDB"]).await {
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

impl ValkeyReservation {
    fn url(&self) -> String {
        format!("redis://localhost:6379/{}", self.database)
    }

    async fn release(self) {
        valkey_command(self.database, &["FLUSHDB"]).await.ok();
        self.lock.close().await.ok();
    }
}

async fn valkey_command(database: u16, arguments: &[&str]) -> Result<(), String> {
    let mut stream = TcpStream::connect("127.0.0.1:6379")
        .await
        .map_err(|error| format!("failed to connect to local Valkey: {error}"))?;
    if database != 0 {
        let selected = send_resp(&mut stream, &["SELECT", &database.to_string()]).await?;
        if selected != "OK" {
            return Err(format!(
                "Valkey refused logical database {database}: {selected}"
            ));
        }
    }
    let reply = send_resp(&mut stream, arguments).await?;
    if reply == "OK" {
        Ok(())
    } else {
        Err(format!("unexpected Valkey response: {reply}"))
    }
}

async fn send_resp(stream: &mut TcpStream, arguments: &[&str]) -> Result<String, String> {
    let mut command = format!("*{}\r\n", arguments.len()).into_bytes();
    for argument in arguments {
        command.extend_from_slice(format!("${}\r\n", argument.len()).as_bytes());
        command.extend_from_slice(argument.as_bytes());
        command.extend_from_slice(b"\r\n");
    }
    stream
        .write_all(&command)
        .await
        .map_err(|error| format!("failed to write a Valkey command: {error}"))?;
    stream
        .flush()
        .await
        .map_err(|error| format!("failed to flush a Valkey command: {error}"))?;
    read_resp(stream).await
}

async fn read_resp(stream: &mut TcpStream) -> Result<String, String> {
    let mut prefix = [0_u8; 1];
    stream
        .read_exact(&mut prefix)
        .await
        .map_err(|error| format!("failed to read a Valkey response: {error}"))?;
    let line = read_resp_line(stream).await?;
    match prefix[0] {
        b'+' => Ok(line),
        b'-' => Err(format!("Valkey returned an error: {line}")),
        other => Err(format!("unsupported Valkey response prefix: {other}")),
    }
}

async fn read_resp_line(stream: &mut TcpStream) -> Result<String, String> {
    let mut bytes = Vec::new();
    loop {
        let mut byte = [0_u8; 1];
        stream
            .read_exact(&mut byte)
            .await
            .map_err(|error| format!("failed to read a Valkey response line: {error}"))?;
        if byte[0] == b'\n' {
            if bytes.last() == Some(&b'\r') {
                bytes.pop();
            }
            return String::from_utf8(bytes)
                .map_err(|error| format!("Valkey returned a non-UTF-8 response: {error}"));
        }
        bytes.push(byte[0]);
        if bytes.len() > 64 * 1024 {
            return Err("Valkey response line exceeded 64 KiB".to_owned());
        }
    }
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

    async fn prepare(&mut self, binary: &Path, database: &str) -> Result<PreparedServer, String> {
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

        let (valkey_url, reservation) = valkey(&self.admin_url).await?;
        self.valkey_reservation = reservation;

        for attempt in 1..=3 {
            let public_port = free_port()?;
            let mut observability_port = free_port()?;
            while observability_port == public_port {
                observability_port = free_port()?;
            }
            let public_origin = format!("http://127.0.0.1:{public_port}");
            let observability_base = format!("http://127.0.0.1:{observability_port}");
            let environment = [
                ("OLP_DATABASE_URL", database.to_owned()),
                ("OLP_VALKEY_URL", valkey_url.clone()),
                ("OLP_LISTEN_ADDR", format!("127.0.0.1:{public_port}")),
                (
                    "OLP_OBSERVABILITY_LISTEN_ADDR",
                    format!("127.0.0.1:{observability_port}"),
                ),
                ("OLP_PUBLIC_ORIGIN", public_origin.clone()),
                ("OLP_CONSOLE_DIR", console_dir.display().to_string()),
                ("OLP_MEDIA_SPOOL_DIR", spool_dir.display().to_string()),
                ("OLP_MASTER_KEY_FILE", master_key_file.display().to_string()),
                (
                    "OLP_AUTH_HMAC_KEY_FILE",
                    auth_hmac_key_file.display().to_string(),
                ),
                (
                    "OLP_BOOTSTRAP_TOKEN_FILE",
                    bootstrap_token_file.display().to_string(),
                ),
                (
                    "OLP_ALLOW_INSECURE_PROVIDER_ENDPOINTS_FOR_TESTS",
                    "test-only".to_owned(),
                ),
                ("RUST_LOG", "olp=info".to_owned()),
            ];

            if attempt == 1 {
                let migrate = Command::new(binary)
                    .arg("migrate")
                    .envs(environment.iter().map(|(key, value)| (*key, value.clone())))
                    .output()
                    .map_err(|error| format!("failed to run olp migrate: {error}"))?;
                if !migrate.status.success() {
                    let stderr = String::from_utf8_lossy(&migrate.stderr);
                    return Err(format!("olp migrate failed ({}): {stderr}", migrate.status));
                }
            }

            self.stderr = Arc::new(Mutex::new(String::new()));
            let mut child = Command::new(binary)
                .arg("all")
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
            )
            .await;
            match startup {
                Ok(()) => {
                    return Ok(PreparedServer {
                        public_origin,
                        observability_base,
                        setup_token,
                        database_url: database.to_owned(),
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
            public_origin: prepared.public_origin,
            observability_base: prepared.observability_base,
            setup_token: prepared.setup_token,
            database_url: prepared.database_url,
            admin_url: self.admin_url,
            database_name: self.database_name,
            run_dir: self.run_dir,
            valkey_reservation: self.valkey_reservation.take(),
        }
    }
}

impl Server {
    pub async fn launch() -> Result<Self, String> {
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
        create_database(&admin, &database_name).await?;

        let run_dir = std::env::temp_dir().join(format!("olp-e2e-{run_token}-{}", random_hex(6)));
        let mut guard = LaunchGuard::new(admin, database_name, run_dir);
        match guard.prepare(&binary, &database).await {
            Ok(prepared) => Ok(guard.into_server(prepared)),
            Err(error) => {
                guard.cleanup().await;
                Err(error)
            }
        }
    }

    pub fn stderr_tail(&self) -> String {
        stderr_tail(&self.stderr)
    }

    /// SIGTERM, bounded wait, then SIGKILL as a last resort.
    pub async fn shutdown(mut self) -> String {
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
                "olp all exited during startup ({status}); stderr:\n{}",
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
                "olp all did not become live within 30s; stderr:\n{}",
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
