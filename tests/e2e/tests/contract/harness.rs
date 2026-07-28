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

pub struct Server {
    child: Child,
    stderr: Arc<Mutex<String>>,
    /// Public listener origin, e.g. `http://127.0.0.1:41234`.
    pub public_origin: String,
    /// Observability listener base.
    pub observability_base: String,
    /// Base64 setup token exactly as written to the bootstrap token file.
    pub setup_token: String,
    admin_url: String,
    database_name: String,
    run_dir: PathBuf,
}

pub fn admin_url() -> String {
    std::env::var("OLP_E2E_DATABASE_ADMIN_URL")
        .unwrap_or_else(|_| "postgres://olp_test:olp_test@localhost:5433/postgres".to_owned())
}

pub fn valkey_url() -> String {
    std::env::var("OLP_E2E_VALKEY_URL").unwrap_or_else(|_| "redis://localhost:6379".to_owned())
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

fn database_url(admin: &str, name: &str) -> String {
    let base = admin.rsplit_once('/').map_or(admin, |(base, _)| base);
    format!("{base}/{name}")
}

impl Server {
    pub async fn launch() -> Result<Self, String> {
        let binary = binary()?;
        let admin = admin_url();
        let database_name = format!("olp_e2e_{}", random_hex(6));
        create_database(&admin, &database_name).await?;
        let database = database_url(&admin, &database_name);

        let run_dir = std::env::temp_dir().join(format!("olp-e2e-{}", random_hex(6)));
        let console_dir = run_dir.join("console");
        let spool_dir = run_dir.join("spool");
        std::fs::create_dir_all(&console_dir)
            .and_then(|()| std::fs::create_dir_all(&spool_dir))
            .map_err(|error| format!("failed to create run directory: {error}"))?;

        let master_key_file = run_dir.join("master-key");
        let auth_hmac_key_file = run_dir.join("auth-hmac-key");
        let bootstrap_token_file = run_dir.join("bootstrap-token");
        let setup_token = random_base64_secret();
        write_secret(&master_key_file, &random_base64_secret())?;
        write_secret(&auth_hmac_key_file, &random_base64_secret())?;
        write_secret(&bootstrap_token_file, &setup_token)?;

        let public_port = free_port()?;
        let observability_port = free_port()?;
        let public_origin = format!("http://127.0.0.1:{public_port}");
        let observability_base = format!("http://127.0.0.1:{observability_port}");
        let valkey = valkey_url();

        let environment = [
            ("OLP_DATABASE_URL", database.clone()),
            ("OLP_VALKEY_URL", valkey),
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

        let migrate = Command::new(&binary)
            .arg("migrate")
            .envs(environment.iter().map(|(key, value)| (*key, value.clone())))
            .output()
            .map_err(|error| format!("failed to run olp migrate: {error}"))?;
        if !migrate.status.success() {
            let stderr = String::from_utf8_lossy(&migrate.stderr);
            drop_database(&admin, &database_name).await;
            return Err(format!("olp migrate failed ({}): {stderr}", migrate.status));
        }

        let mut child = Command::new(&binary)
            .arg("all")
            .envs(environment.iter().map(|(key, value)| (*key, value.clone())))
            .stdout(Stdio::null())
            .stderr(Stdio::piped())
            .spawn()
            .map_err(|error| format!("failed to spawn olp all: {error}"))?;

        let stderr = Arc::new(Mutex::new(String::new()));
        if let Some(pipe) = child.stderr.take() {
            let sink = Arc::clone(&stderr);
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

        let mut server = Self {
            child,
            stderr,
            public_origin,
            observability_base,
            setup_token,
            admin_url: admin,
            database_name,
            run_dir,
        };
        server.await_live().await?;
        Ok(server)
    }

    async fn await_live(&mut self) -> Result<(), String> {
        let client = reqwest::Client::new();
        let url = format!("{}/health/live", self.observability_base);
        let deadline = Instant::now() + Duration::from_secs(30);
        loop {
            if let Ok(Some(status)) = self.child.try_wait() {
                return Err(format!(
                    "olp all exited during startup ({status}); stderr:\n{}",
                    self.stderr_tail()
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
                    self.stderr_tail()
                ));
            }
            tokio::time::sleep(Duration::from_millis(200)).await;
        }
    }

    pub fn stderr_tail(&self) -> String {
        let stderr = self.stderr.lock().unwrap();
        let tail_start = stderr.len().saturating_sub(8_192);
        stderr[tail_start..].to_owned()
    }

    /// SIGTERM, bounded wait, then SIGKILL as a last resort.
    pub async fn shutdown(mut self) -> String {
        let pid = self.child.id().to_string();
        Command::new("kill").args(["-TERM", &pid]).status().ok();
        let deadline = Instant::now() + Duration::from_secs(10);
        loop {
            match self.child.try_wait() {
                Ok(Some(_)) => break,
                Ok(None) if Instant::now() > deadline => {
                    self.child.kill().ok();
                    self.child.wait().ok();
                    break;
                }
                Ok(None) => tokio::time::sleep(Duration::from_millis(100)).await,
                Err(_) => break,
            }
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
}
