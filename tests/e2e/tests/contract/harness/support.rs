use std::io::Read as _;
use std::path::{Path, PathBuf};
use std::process::{Child, Command};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use base64::Engine as _;
use rand::RngCore as _;
use sqlx::Connection as _;

pub(super) fn binary() -> Result<PathBuf, String> {
    let path = std::env::var("OLP_E2E_BIN")
        .map_err(|_| "OLP_E2E_BIN is unset; run via scripts/run-e2e-tests.sh".to_owned())?;
    let path = PathBuf::from(path);
    if !path.is_file() {
        return Err(format!("OLP_E2E_BIN does not exist: {}", path.display()));
    }
    Ok(path)
}

pub(super) fn random_hex(bytes: usize) -> String {
    let mut buffer = vec![0_u8; bytes];
    rand::rng().fill_bytes(&mut buffer);
    crate::otlp::hex_id(&buffer)
}

pub(super) fn random_base64_secret() -> String {
    let mut buffer = [0_u8; 32];
    rand::rng().fill_bytes(&mut buffer);
    base64::engine::general_purpose::STANDARD.encode(buffer)
}

pub(super) fn write_secret(path: &Path, contents: &str) -> Result<(), String> {
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

pub(super) fn process_environment(
    run_dir: &Path,
    database: &str,
    valkey: &str,
    public_origin: String,
    observability_base: String,
    bootstrap: bool,
    tracing_endpoint: Option<&str>,
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
            "OLP_PROVIDER_EGRESS_ALLOW_CIDRS",
            "127.0.0.0/8,::1/128".to_owned(),
        ),
        (
            "OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS",
            "127.0.0.1,localhost".to_owned(),
        ),
        ("RUST_LOG", "olp=info".to_owned()),
    ];
    if bootstrap {
        environment.push(("OLP_BOOTSTRAP_TOKEN_FILE", path("bootstrap-token")));
    }
    if let Some(endpoint) = tracing_endpoint {
        environment.push(("OLP_OTLP_TRACES_ENDPOINT", endpoint.to_owned()));
        environment.push(("OLP_TRACE_SAMPLE_RATIO", "0".to_owned()));
    }
    environment
}

pub(super) fn run_migrate(
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

pub(super) fn free_port() -> Result<u16, String> {
    let listener = std::net::TcpListener::bind("127.0.0.1:0")
        .map_err(|error| format!("failed to bind an ephemeral port: {error}"))?;
    let port = listener
        .local_addr()
        .map_err(|error| format!("failed to read the ephemeral port: {error}"))?
        .port();
    drop(listener);
    Ok(port)
}

pub(super) async fn create_database(admin: &str, name: &str) -> Result<(), String> {
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

pub(super) async fn drop_database(admin: &str, name: &str) {
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

pub(super) async fn await_path_or_exit(
    child: &mut Child,
    path: &Path,
    process: &str,
) -> Result<(), String> {
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

pub(super) fn capture_stderr(pipe: impl std::io::Read + Send + 'static, sink: Arc<Mutex<String>>) {
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

pub(super) async fn await_live(
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

pub(super) fn stderr_tail(stderr: &Arc<Mutex<String>>) -> String {
    let stderr = stderr.lock().unwrap();
    let mut tail_start = stderr.len().saturating_sub(8_192);
    while !stderr.is_char_boundary(tail_start) {
        tail_start += 1;
    }
    stderr[tail_start..].to_owned()
}

pub(super) async fn terminate_child(child: &mut Child) {
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
