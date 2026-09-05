use std::{
    net::{IpAddr, SocketAddr},
    path::PathBuf,
};

use clap::{Args, Parser, Subcommand};
use ipnet::IpNet;
use olp_engine::providers::{connector::ResponseLimits, http_egress::EgressPolicy};

use crate::public_http::{
    body_limits::BodyLimits,
    cors::CorsAllowedOrigins,
    proxy::TrustedProxyCidr,
    public_origin::PublicOrigin,
    request_admission::public::{
        DEFAULT_MAX_IN_FLIGHT_INFERENCE_REQUESTS, DEFAULT_MAX_IN_FLIGHT_MANAGEMENT_REQUESTS,
        MAX_ADMISSION_CAPACITY,
    },
};

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
    /// Check the loopback readiness endpoint and exit successfully only when ready.
    HealthProbe,
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

/// Filesystem assets shared verbatim between the serve modes and `doctor`,
/// so the doctor validates exactly what the server will load.
#[derive(Clone, Debug, Args)]
pub(super) struct RuntimeAssetArgs {
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
    /// JSON file mapping runtime provider IDs to credential files. The JSON
    /// contains paths, never credential values.
    #[arg(long, env = "OLP_CONNECTOR_CONFIG_FILE")]
    pub(super) connector_config_file: Option<PathBuf>,
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
    /// Maximum inference requests admitted process-wide until response completion.
    #[arg(
        long,
        env = "OLP_HTTP_MAX_IN_FLIGHT_INFERENCE_REQUESTS",
        default_value_t = DEFAULT_MAX_IN_FLIGHT_INFERENCE_REQUESTS,
        value_parser = parse_admission_capacity
    )]
    pub(super) http_max_in_flight_inference_requests: usize,
    /// Reserved management requests admitted process-wide until response completion.
    #[arg(
        long,
        env = "OLP_HTTP_MAX_IN_FLIGHT_MANAGEMENT_REQUESTS",
        default_value_t = DEFAULT_MAX_IN_FLIGHT_MANAGEMENT_REQUESTS,
        value_parser = parse_admission_capacity
    )]
    pub(super) http_max_in_flight_management_requests: usize,
    /// Age after which an HTTP/2 connection receives GOAWAY so clients rebalance.
    #[arg(
        long,
        env = "OLP_HTTP_CONNECTION_MAX_AGE_SECONDS",
        default_value_t = 300,
        value_parser = parse_connection_max_age_seconds
    )]
    pub(super) http_connection_max_age_seconds: u64,
    /// Grace period for a draining connection; extended while responses stream.
    #[arg(
        long,
        env = "OLP_HTTP_CONNECTION_DRAIN_TIMEOUT_SECONDS",
        default_value_t = 30,
        value_parser = parse_connection_drain_timeout_seconds
    )]
    pub(super) http_connection_drain_timeout_seconds: u64,
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
    #[command(flatten)]
    pub(super) assets: RuntimeAssetArgs,
    #[arg(long, env = "OLP_AUTH_HMAC_KEY_FILE")]
    pub(super) auth_hmac_key_file: Option<PathBuf>,
    /// Base64-encoded one-time setup token, mounted only in control-plane pods.
    #[arg(long, env = "OLP_BOOTSTRAP_TOKEN_FILE")]
    pub(super) bootstrap_token_file: Option<PathBuf>,
    /// Comma-separated CIDRs for reverse proxies allowed to supply
    /// X-Forwarded-For for unauthenticated authentication admission. An empty
    /// value (the shipped default) means forwarding headers are ignored.
    #[arg(
        long,
        env = "OLP_TRUSTED_PROXY_CIDRS",
        default_value = "",
        hide_default_value = true
    )]
    pub(super) trusted_proxy_cidrs: TrustedProxyCidrs,
    /// Comma-separated browser origins allowed to call the inference gateway
    /// cross-origin. Empty (the default) disables CORS; wildcards are refused.
    #[arg(
        long,
        env = "OLP_GATEWAY_CORS_ALLOWED_ORIGINS",
        default_value = "",
        hide_default_value = true
    )]
    pub(super) gateway_cors_allowed_origins: CorsAllowedOrigins,
    #[command(flatten)]
    pub(super) provider_egress: ProviderEgressArgs,
    #[command(flatten)]
    pub(super) provider_response_limits: ProviderResponseLimitArgs,
    #[command(flatten)]
    pub(super) body_limits: BodyLimitArgs,
    #[arg(long, env = "OLP_MASTER_KEY_FILE")]
    pub(super) master_key_file: Option<PathBuf>,
    #[command(flatten)]
    pub(super) tracing: TracingArgs,
}

#[derive(Clone, Debug, Args)]
pub(super) struct TracingArgs {
    #[arg(long, env = "OLP_OTLP_TRACES_ENDPOINT")]
    pub(super) otlp_traces_endpoint: Option<String>,
    #[arg(long, env = "OLP_OTLP_HEADERS_FILE")]
    pub(super) otlp_headers_file: Option<PathBuf>,
    #[arg(
        long,
        env = "OLP_TRACE_SAMPLE_RATIO",
        default_value_t = 1.0,
        value_parser = parse_trace_sample_ratio
    )]
    pub(super) trace_sample_ratio: f64,
    #[arg(
        long,
        env = "OLP_TRACE_PROPAGATE_UPSTREAM",
        default_value = "true",
        action = clap::ArgAction::Set
    )]
    pub(super) trace_propagate_upstream: bool,
    #[arg(
        long,
        env = "OLP_TRACE_ACCEPT_INBOUND",
        default_value = "true",
        action = clap::ArgAction::Set
    )]
    pub(super) trace_accept_inbound: bool,
}

#[derive(Clone, Debug, Args)]
pub(super) struct BodyLimitArgs {
    /// Largest JSON request body, before and after gzip inflation.
    #[arg(
        long,
        env = "OLP_HTTP_MAX_JSON_BODY_BYTES",
        default_value_t = BodyLimits::default().json_body_bytes,
        value_parser = parse_json_body_bytes
    )]
    pub(super) http_max_json_body_bytes: usize,
    /// Largest raw or multipart media request body; must stay within half
    /// of the media spool capacity.
    #[arg(
        long,
        env = "OLP_HTTP_MAX_MEDIA_BODY_BYTES",
        default_value_t = BodyLimits::default().media_body_bytes,
        value_parser = parse_media_body_bytes
    )]
    pub(super) http_max_media_body_bytes: usize,
    /// Inline base64 media items accepted per JSON request.
    #[arg(
        long,
        env = "OLP_HTTP_MAX_INLINE_MEDIA_ITEMS",
        default_value_t = BodyLimits::default().inline_media_items,
        value_parser = parse_inline_media_items
    )]
    pub(super) http_max_inline_media_items: usize,
    /// Decoded size cap for one inline media item.
    #[arg(
        long,
        env = "OLP_HTTP_MAX_INLINE_MEDIA_ITEM_BYTES",
        default_value_t = BodyLimits::default().inline_media_item_bytes,
        value_parser = parse_inline_media_bytes
    )]
    pub(super) http_max_inline_media_item_bytes: usize,
    /// Decoded size cap for all inline media in one request.
    #[arg(
        long,
        env = "OLP_HTTP_MAX_INLINE_MEDIA_TOTAL_BYTES",
        default_value_t = BodyLimits::default().inline_media_total_bytes,
        value_parser = parse_inline_media_bytes
    )]
    pub(super) http_max_inline_media_total_bytes: usize,
}

impl BodyLimitArgs {
    pub(super) const fn limits(&self) -> BodyLimits {
        BodyLimits {
            json_body_bytes: self.http_max_json_body_bytes,
            media_body_bytes: self.http_max_media_body_bytes,
            inline_media_items: self.http_max_inline_media_items,
            inline_media_item_bytes: self.http_max_inline_media_item_bytes,
            inline_media_total_bytes: self.http_max_inline_media_total_bytes,
        }
    }
}

#[derive(Clone, Debug, Args)]
pub(super) struct ProviderResponseLimitArgs {
    /// Largest provider response body buffered for non-streaming operations.
    #[arg(
        long,
        env = "OLP_PROVIDER_MAX_RESPONSE_BYTES",
        default_value_t = ResponseLimits::default().max_response_bytes,
        value_parser = parse_provider_response_bytes
    )]
    pub(super) provider_max_response_bytes: usize,
    /// Largest single streamed provider event; must not exceed the response cap.
    #[arg(
        long,
        env = "OLP_PROVIDER_MAX_EVENT_BYTES",
        default_value_t = ResponseLimits::default().max_event_bytes,
        value_parser = parse_provider_event_bytes
    )]
    pub(super) provider_max_event_bytes: usize,
}

impl ProviderResponseLimitArgs {
    pub(super) fn limits(&self) -> Result<ResponseLimits, String> {
        if self.provider_max_event_bytes > self.provider_max_response_bytes {
            return Err(
                "OLP_PROVIDER_MAX_EVENT_BYTES must not exceed OLP_PROVIDER_MAX_RESPONSE_BYTES"
                    .to_owned(),
            );
        }
        Ok(ResponseLimits {
            max_response_bytes: self.provider_max_response_bytes,
            max_event_bytes: self.provider_max_event_bytes,
        })
    }
}

const KIB: usize = 1024;
const MIB: usize = 1024 * KIB;

fn parse_json_body_bytes(value: &str) -> Result<usize, String> {
    parse_bytes_in(value, 64 * KIB, 64 * MIB, "JSON body limit")
}

fn parse_media_body_bytes(value: &str) -> Result<usize, String> {
    parse_bytes_in(value, MIB, 1024 * MIB, "media body limit")
}

fn parse_inline_media_items(value: &str) -> Result<usize, String> {
    parse_bytes_in(value, 1, 64, "inline media item count")
}

fn parse_inline_media_bytes(value: &str) -> Result<usize, String> {
    parse_bytes_in(value, KIB, 64 * MIB, "inline media limit")
}

fn parse_provider_response_bytes(value: &str) -> Result<usize, String> {
    parse_bytes_in(value, MIB, 256 * MIB, "provider response limit")
}

fn parse_provider_event_bytes(value: &str) -> Result<usize, String> {
    parse_bytes_in(value, 64 * KIB, 256 * MIB, "provider event limit")
}

fn parse_trace_sample_ratio(value: &str) -> Result<f64, String> {
    let ratio = value
        .parse::<f64>()
        .map_err(|_| "trace sample ratio must be a number".to_owned())?;
    if ratio.is_finite() && (0.0..=1.0).contains(&ratio) {
        Ok(ratio)
    } else {
        Err("trace sample ratio must be between 0.0 and 1.0".to_owned())
    }
}

fn parse_bytes_in(value: &str, min: usize, max: usize, label: &str) -> Result<usize, String> {
    let bytes = value
        .parse::<usize>()
        .map_err(|_| format!("{label} must be an integer"))?;
    if !(min..=max).contains(&bytes) {
        return Err(format!("{label} must be between {min} and {max}"));
    }
    Ok(bytes)
}

#[derive(Clone, Debug, Args)]
pub(super) struct ProviderEgressArgs {
    /// Comma-separated CIDRs exempt from the non-public egress denylist for
    /// provider endpoints. Applied to literal hosts and every resolved
    /// address. Empty (the default) keeps the public-only policy.
    #[arg(
        long,
        env = "OLP_PROVIDER_EGRESS_ALLOW_CIDRS",
        default_value = "",
        hide_default_value = true
    )]
    pub(super) provider_egress_allow_cidrs: ProviderEgressAllowCidrs,
    /// Comma-separated hostnames or IP literals whose provider endpoints may
    /// use plain HTTP. Empty (the default) requires HTTPS everywhere.
    #[arg(
        long,
        env = "OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS",
        default_value = "",
        hide_default_value = true
    )]
    pub(super) provider_egress_allow_http_hosts: ProviderEgressAllowHttpHosts,
}

impl ProviderEgressArgs {
    pub(super) fn policy(&self) -> EgressPolicy {
        EgressPolicy::new(
            self.provider_egress_allow_cidrs.0.clone(),
            self.provider_egress_allow_http_hosts.0.clone(),
        )
    }
}

fn parse_admission_capacity(value: &str) -> Result<usize, String> {
    let capacity = value
        .parse::<usize>()
        .map_err(|_| "admission capacity must be an integer".to_owned())?;
    if !(1..=MAX_ADMISSION_CAPACITY).contains(&capacity) {
        return Err(format!(
            "admission capacity must be between 1 and {MAX_ADMISSION_CAPACITY}"
        ));
    }
    Ok(capacity)
}

fn parse_connection_max_age_seconds(value: &str) -> Result<u64, String> {
    parse_seconds_in(value, 1, 86_400, "connection max age")
}

fn parse_connection_drain_timeout_seconds(value: &str) -> Result<u64, String> {
    parse_seconds_in(value, 1, 3_600, "connection drain timeout")
}

fn parse_seconds_in(value: &str, min: u64, max: u64, label: &str) -> Result<u64, String> {
    let seconds = value
        .parse::<u64>()
        .map_err(|_| format!("{label} must be an integer number of seconds"))?;
    if !(min..=max).contains(&seconds) {
        return Err(format!("{label} must be between {min} and {max} seconds"));
    }
    Ok(seconds)
}

#[derive(Clone, Debug, Args)]
pub(super) struct DoctorArgs {
    #[command(flatten)]
    pub(super) persistence: PersistenceArgs,
    #[command(flatten)]
    pub(super) assets: RuntimeAssetArgs,
    #[arg(long, env = "OLP_MASTER_KEY_FILE")]
    pub(super) master_key_file: PathBuf,
    #[arg(long, env = "OLP_AUTH_HMAC_KEY_FILE")]
    pub(super) auth_hmac_key_file: PathBuf,
    #[command(flatten)]
    pub(super) provider_egress: ProviderEgressArgs,
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

/// Comma-separated trusted-proxy CIDR list. Unlike a bare `Vec` with clap's
/// `value_delimiter`, an empty value (the shipped `OLP_TRUSTED_PROXY_CIDRS=`
/// default) parses to an empty list instead of failing startup.
#[derive(Clone, Debug, Default)]
pub(super) struct TrustedProxyCidrs(pub(super) Vec<TrustedProxyCidr>);

impl std::ops::Deref for TrustedProxyCidrs {
    type Target = [TrustedProxyCidr];

    fn deref(&self) -> &Self::Target {
        &self.0
    }
}

impl std::str::FromStr for TrustedProxyCidrs {
    type Err = crate::public_http::proxy::TrustedProxyCidrParseError;

    fn from_str(value: &str) -> Result<Self, Self::Err> {
        value
            .split(',')
            .map(str::trim)
            .filter(|part| !part.is_empty())
            .map(TrustedProxyCidr::from_str)
            .collect::<Result<Vec<_>, _>>()
            .map(Self)
    }
}

#[derive(Clone, Debug, Default)]
pub(super) struct ProviderEgressAllowCidrs(pub(super) Vec<IpNet>);

impl std::str::FromStr for ProviderEgressAllowCidrs {
    type Err = String;

    fn from_str(value: &str) -> Result<Self, Self::Err> {
        value
            .split(',')
            .map(str::trim)
            .filter(|part| !part.is_empty())
            .map(|part| {
                part.parse::<IpNet>()
                    .map_err(|_| format!("provider egress CIDR `{part}` is invalid"))
            })
            .collect::<Result<Vec<_>, _>>()
            .map(Self)
    }
}

#[derive(Clone, Debug, Default)]
pub(super) struct ProviderEgressAllowHttpHosts(pub(super) Vec<String>);

impl std::str::FromStr for ProviderEgressAllowHttpHosts {
    type Err = String;

    fn from_str(value: &str) -> Result<Self, Self::Err> {
        value
            .split(',')
            .map(str::trim)
            .filter(|part| !part.is_empty())
            .map(parse_plain_http_host)
            .collect::<Result<Vec<_>, _>>()
            .map(Self)
    }
}

fn parse_plain_http_host(value: &str) -> Result<String, String> {
    let literal = value.trim_start_matches('[').trim_end_matches(']');
    if let Ok(address) = literal.parse::<IpAddr>() {
        return Ok(address.to_string());
    }
    let hostname_shaped = !value.is_empty()
        && value.len() <= 253
        && !value.starts_with(['-', '.'])
        && !value.ends_with(['-', '.'])
        && value.bytes().all(|byte| {
            byte.is_ascii_lowercase() || byte.is_ascii_digit() || byte == b'-' || byte == b'.'
        });
    if !hostname_shaped {
        return Err(format!(
            "provider egress HTTP host `{value}` must be a lowercase hostname or an IP literal"
        ));
    }
    Ok(value.to_owned())
}
