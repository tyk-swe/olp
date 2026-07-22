use std::{net::SocketAddr, path::PathBuf};

use clap::{Args, Parser, Subcommand};

use crate::{PublicOrigin, TrustedProxyCidr};

#[derive(Debug, Parser)]
#[command(name = "olp", version, about = "OpenLLMProxy")]
pub(super) struct Cli {
    #[command(subcommand)]
    pub(super) command: Command,
}

#[derive(Debug, Subcommand)]
pub(super) enum Command {
    /// Run gateway, control plane, and background outbox worker together.
    All(ServeArgs),
    /// Run only inference, probes, and metrics.
    Gateway(ServeArgs),
    /// Run only management API, probes, metrics, and static console.
    Control(ServeArgs),
    /// Publish outbox hints and perform asynchronous persistence work.
    Worker(PersistenceArgs),
    /// Verify the legacy Valkey stream, apply PostgreSQL migrations, and exit.
    Migrate(MigrateArgs),
    /// Validate dependencies and mounted secrets, then exit.
    Doctor(DoctorArgs),
    /// Inspect, re-encrypt, and verify retirement of master-key versions.
    MasterKey(MasterKeyArgs),
    /// Internal shell-free Kubernetes pre-stop delay.
    #[command(hide = true)]
    InternalPreStop(InternalPreStopArgs),
}

#[derive(Clone, Debug, Args)]
pub(super) struct InternalPreStopArgs {
    #[arg(long, default_value_t = 10)]
    pub(super) seconds: u64,
}

#[derive(Clone, Debug, Args)]
pub(super) struct DatabaseArgs {
    #[arg(long, env = "OLP_DATABASE_URL")]
    pub(super) database_url: String,
    #[arg(long, env = "OLP_DATABASE_MAX_CONNECTIONS", default_value_t = 20)]
    pub(super) database_max_connections: u32,
}

#[derive(Clone, Debug, Args)]
pub(super) struct PersistenceArgs {
    #[command(flatten)]
    pub(super) database: DatabaseArgs,
    #[arg(long, env = "OLP_VALKEY_URL")]
    pub(super) valkey_url: String,
}

#[derive(Clone, Debug, Args)]
pub(super) struct MigrateArgs {
    #[command(flatten)]
    pub(super) persistence: PersistenceArgs,
    /// Test-only target used to construct an N-1 upgrade fixture.
    #[arg(long, hide = true)]
    pub(super) through_version: Option<i64>,
}

#[derive(Clone, Debug, Args)]
pub(super) struct ServeArgs {
    #[command(flatten)]
    pub(super) database: DatabaseArgs,
    #[arg(long, env = "OLP_VALKEY_URL")]
    pub(super) valkey_url: Option<String>,
    #[arg(long, env = "OLP_LISTEN_ADDR", default_value = "127.0.0.1:8080")]
    pub(super) listen_addr: SocketAddr,
    /// Private listener for probes and Prometheus metrics. Keep the default
    /// loopback-only unless an internal network is intentionally configured.
    #[arg(
        long,
        env = "OLP_OBSERVABILITY_LISTEN_ADDR",
        default_value = "127.0.0.1:9090"
    )]
    pub(super) observability_listen_addr: SocketAddr,
    /// Maximum simultaneously admitted TCP connections per HTTP listener.
    #[arg(long, env = "OLP_HTTP_MAX_CONNECTIONS", default_value_t = 1024)]
    pub(super) http_max_connections: usize,
    #[arg(
        long,
        env = "OLP_PUBLIC_ORIGIN",
        default_value = "http://127.0.0.1:8080"
    )]
    pub(super) public_origin: PublicOrigin,
    /// Whether password-based local sign-in is exposed after installation setup.
    #[arg(
        long,
        env = "OLP_LOCAL_LOGIN_ENABLED",
        default_value = "true",
        action = clap::ArgAction::Set
    )]
    pub(super) local_login_enabled: bool,
    #[arg(long, env = "OLP_CONSOLE_DIR", default_value = "console/build")]
    pub(super) console_dir: PathBuf,
    #[arg(long, env = "OLP_MEDIA_SPOOL_DIR")]
    pub(super) media_spool_dir: Option<PathBuf>,
    #[arg(
        long,
        env = "OLP_MEDIA_SPOOL_CAPACITY_BYTES",
        default_value_t = 1_073_741_824_u64
    )]
    pub(super) media_spool_capacity_bytes: u64,
    #[arg(long, env = "OLP_AUTH_HMAC_KEY_FILE")]
    pub(super) auth_hmac_key_file: Option<PathBuf>,
    /// Base64-encoded one-time setup token, mounted only in control-plane pods.
    #[arg(long, env = "OLP_BOOTSTRAP_TOKEN_FILE")]
    pub(super) bootstrap_token_file: Option<PathBuf>,
    /// Comma-separated CIDRs for reverse proxies allowed to supply
    /// X-Forwarded-For for unauthenticated authentication admission.
    #[arg(long, env = "OLP_TRUSTED_PROXY_CIDRS", value_delimiter = ',')]
    pub(super) trusted_proxy_cidrs: Vec<TrustedProxyCidr>,
    #[arg(long, env = "OLP_MASTER_KEY_FILE")]
    pub(super) master_key_file: Option<PathBuf>,
    /// JSON file mapping runtime provider IDs to credential files. The JSON
    /// contains paths, never credential values.
    #[arg(long, env = "OLP_CONNECTOR_CONFIG_FILE")]
    pub(super) connector_config_file: Option<PathBuf>,
}

#[derive(Clone, Debug, Args)]
pub(super) struct DoctorArgs {
    #[command(flatten)]
    pub(super) persistence: PersistenceArgs,
    #[arg(long, env = "OLP_CONSOLE_DIR", default_value = "console/build")]
    pub(super) console_dir: PathBuf,
    #[arg(long, env = "OLP_MEDIA_SPOOL_DIR")]
    pub(super) media_spool_dir: Option<PathBuf>,
    #[arg(
        long,
        env = "OLP_MEDIA_SPOOL_CAPACITY_BYTES",
        default_value_t = 1_073_741_824_u64
    )]
    pub(super) media_spool_capacity_bytes: u64,
    #[arg(long, env = "OLP_MASTER_KEY_FILE")]
    pub(super) master_key_file: PathBuf,
    #[arg(long, env = "OLP_AUTH_HMAC_KEY_FILE")]
    pub(super) auth_hmac_key_file: PathBuf,
    #[arg(long, env = "OLP_CONNECTOR_CONFIG_FILE")]
    pub(super) connector_config_file: Option<PathBuf>,
}

#[derive(Clone, Debug, Args)]
pub(super) struct MasterKeyArgs {
    #[command(flatten)]
    pub(super) database: DatabaseArgs,
    #[arg(long, env = "OLP_MASTER_KEY_FILE")]
    pub(super) master_key_file: PathBuf,
    #[command(subcommand)]
    pub(super) action: MasterKeyAction,
}

#[derive(Clone, Debug, Subcommand)]
pub(super) enum MasterKeyAction {
    /// Count and authenticate every encrypted envelope without changing rows.
    Status {
        #[arg(long, default_value_t = 100)]
        batch_size: u16,
    },
    /// Re-encrypt non-active envelopes in resumable transactional batches.
    Reencrypt {
        #[arg(long, default_value_t = 100)]
        batch_size: u16,
        /// Authenticate all rows and report work without updating ciphertext.
        #[arg(long)]
        dry_run: bool,
    },
    /// Fail unless a decrypt-only version has zero remaining references.
    VerifyRetirement {
        #[arg(long)]
        version: u32,
        #[arg(long, default_value_t = 100)]
        batch_size: u16,
    },
}
