use std::{
    collections::HashMap,
    fmt,
    path::PathBuf,
    sync::atomic::{AtomicU64, Ordering},
    time::Duration,
};

use opentelemetry::{KeyValue, global, trace::TracerProvider as _};
use opentelemetry_otlp::{WithExportConfig as _, WithHttpConfig as _};
use opentelemetry_sdk::{
    Resource,
    error::OTelSdkResult,
    propagation::TraceContextPropagator,
    trace::{Sampler, SdkTracerProvider, SpanData, SpanExporter},
};
use tracing_subscriber::{
    EnvFilter, Layer as _, filter::filter_fn, layer::SubscriberExt as _,
    util::SubscriberInitExt as _,
};

use olp_engine::inference::tracing::OTEL_TARGET;

use crate::{
    bootstrap::cli::validation::{check_secret_permissions, read_secret_file},
    bootstrap::{cli::AppResult, state::ApiMode},
};

mod processor;
mod request;

use processor::BoundedSpanProcessor;
pub(crate) use request::trace_admitted_request;

const EXPORT_TIMEOUT: Duration = Duration::from_secs(2);
const EXPORT_QUEUE_CAPACITY: usize = 2_048;
const EXPORT_BATCH_SIZE: usize = 256;
const EXPORT_SCHEDULE_DELAY: Duration = Duration::from_millis(200);
const AMBIENT_OTLP_HEADER_VARIABLES: [&str; 2] = [
    "OTEL_EXPORTER_OTLP_TRACES_HEADERS",
    "OTEL_EXPORTER_OTLP_HEADERS",
];
static TRACE_EXPORT_DROPPED_TOTAL: AtomicU64 = AtomicU64::new(0);

pub(crate) struct Config {
    pub(crate) endpoint: Option<String>,
    pub(crate) headers_file: Option<PathBuf>,
    pub(crate) sample_ratio: f64,
    pub(crate) propagate_upstream: bool,
    pub(crate) accept_inbound: bool,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct RuntimeConfig {
    pub(crate) propagate_upstream: bool,
    pub(crate) accept_inbound: bool,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct RequestConfig {
    pub(crate) installation_id: uuid::Uuid,
    pub(crate) propagate_upstream: bool,
    pub(crate) accept_inbound: bool,
}

pub(crate) struct Handle {
    provider: Option<SdkTracerProvider>,
    runtime: RuntimeConfig,
}

impl RuntimeConfig {
    #[must_use]
    pub(crate) const fn for_installation(self, installation_id: uuid::Uuid) -> RequestConfig {
        RequestConfig {
            installation_id,
            propagate_upstream: self.propagate_upstream,
            accept_inbound: self.accept_inbound,
        }
    }
}

struct CountingExporter {
    inner: opentelemetry_otlp::SpanExporter,
}

struct ExportDropGuard {
    count: u64,
    armed: bool,
}

impl Drop for ExportDropGuard {
    fn drop(&mut self) {
        if self.armed {
            record_export_drops(self.count);
        }
    }
}

impl fmt::Debug for CountingExporter {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("CountingExporter")
    }
}

impl SpanExporter for CountingExporter {
    async fn export(&self, batch: Vec<SpanData>) -> OTelSdkResult {
        let mut guard = ExportDropGuard {
            count: u64::try_from(batch.len()).unwrap_or(u64::MAX),
            armed: true,
        };
        let result = self.inner.export(batch).await;
        if result.is_ok() {
            guard.armed = false;
        }
        result
    }

    fn shutdown_with_timeout(&self, timeout: Duration) -> OTelSdkResult {
        self.inner.shutdown_with_timeout(timeout)
    }

    fn force_flush(&self) -> OTelSdkResult {
        self.inner.force_flush()
    }

    fn set_resource(&mut self, resource: &Resource) {
        self.inner.set_resource(resource);
    }
}

impl Handle {
    pub(crate) async fn install(config: Config, mode: ApiMode) -> AppResult<Self> {
        let Some(endpoint) = config.endpoint.as_deref() else {
            install_logging()?;
            return Ok(Self {
                provider: None,
                runtime: RuntimeConfig {
                    propagate_upstream: config.propagate_upstream,
                    accept_inbound: config.accept_inbound,
                },
            });
        };
        validate_endpoint(endpoint)?;
        let headers = load_headers(config.headers_file.as_ref()).await?;
        let provider = build_provider(endpoint, headers, config.sample_ratio, mode)?;
        global::set_text_map_propagator(TraceContextPropagator::new());
        install_tracing(&provider)?;
        Ok(Self {
            provider: Some(provider),
            runtime: RuntimeConfig {
                propagate_upstream: config.propagate_upstream,
                accept_inbound: config.accept_inbound,
            },
        })
    }

    /// The per-request configuration, or `None` when no exporter is
    /// installed. Callers gate on this rather than re-deriving "tracing is
    /// on" from a separate flag.
    #[must_use]
    pub(crate) const fn runtime(&self) -> Option<RuntimeConfig> {
        if self.provider.is_some() {
            Some(self.runtime)
        } else {
            None
        }
    }

    pub(crate) async fn shutdown(self) -> AppResult<()> {
        let Some(provider) = self.provider else {
            return Ok(());
        };
        match tokio::task::spawn_blocking(move || provider.shutdown()).await {
            Ok(Ok(())) => {}
            Ok(Err(_)) => ::tracing::warn!("trace exporter did not flush cleanly during shutdown"),
            Err(error) => return Err(std::io::Error::other(error).into()),
        }
        Ok(())
    }
}

pub(crate) fn install_logging() -> AppResult<()> {
    install(None::<tracing_subscriber::layer::Identity>)
}

fn install_tracing(provider: &SdkTracerProvider) -> AppResult<()> {
    let tracer = provider.tracer("openllmproxy");
    install(Some(
        tracing_opentelemetry::layer()
            .with_tracer(tracer)
            .with_location(false)
            .with_tracked_inactivity(false)
            .with_threads(false)
            .with_target(false)
            // Spans are parented explicitly (`set_parent` inbound, `parent:`
            // per attempt), and the only reader of the ambient context goes
            // through `Span::current()`, so activating an OTel context on
            // every span enter would buy nothing and cost a mutex per
            // streamed body poll.
            .with_context_activation(false)
            .with_error_fields_to_exceptions(false)
            .with_error_records_to_exceptions(false)
            .with_error_events_to_exceptions(false)
            .with_error_events_to_status(false)
            .with_filter(filter_fn(|metadata| metadata.target() == OTEL_TARGET)),
    ))
}

fn install<L>(telemetry: Option<L>) -> AppResult<()>
where
    L: tracing_subscriber::Layer<tracing_subscriber::Registry> + Send + Sync + 'static,
{
    tracing_subscriber::registry()
        .with(telemetry)
        .with(
            tracing_subscriber::fmt::layer()
                .json()
                .with_filter(log_filter())
                // `EnvFilter` matches targets by prefix, so an "olp" directive
                // also selects "olp.telemetry". Telemetry spans are never
                // emitted as log lines, and letting the JSON layer track them
                // would make every attribute record re-parse and re-serialize
                // the span's accumulated fields.
                .with_filter(filter_fn(|metadata| metadata.target() != OTEL_TARGET)),
        )
        .try_init()?;
    Ok(())
}

fn log_filter() -> EnvFilter {
    EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("olp=info"))
}

fn build_provider(
    endpoint: &str,
    headers: HashMap<String, String>,
    sample_ratio: f64,
    mode: ApiMode,
) -> AppResult<SdkTracerProvider> {
    reject_ambient_otlp_headers(|name| std::env::var_os(name))?;
    let exporter = opentelemetry_otlp::SpanExporter::builder()
        .with_http()
        .with_endpoint(endpoint)
        .with_timeout(EXPORT_TIMEOUT)
        .with_headers(headers)
        .build()?;
    let batch = BoundedSpanProcessor::new(
        CountingExporter { inner: exporter },
        EXPORT_QUEUE_CAPACITY,
        EXPORT_BATCH_SIZE,
        EXPORT_SCHEDULE_DELAY,
    );
    Ok(SdkTracerProvider::builder()
        .with_span_processor(batch)
        .with_sampler(Sampler::ParentBased(Box::new(Sampler::TraceIdRatioBased(
            sample_ratio,
        ))))
        .with_resource(
            Resource::builder_empty()
                .with_service_name("openllmproxy")
                .with_attributes([
                    KeyValue::new("service.version", env!("CARGO_PKG_VERSION")),
                    KeyValue::new("olp.process.mode", mode.to_string()),
                ])
                .build(),
        )
        .build())
}

fn reject_ambient_otlp_headers(
    mut environment_value: impl FnMut(&str) -> Option<std::ffi::OsString>,
) -> AppResult<()> {
    for name in AMBIENT_OTLP_HEADER_VARIABLES {
        if environment_value(name).is_some() {
            return Err(std::io::Error::other(format!(
                "{name} is not supported; use OLP_OTLP_HEADERS_FILE"
            ))
            .into());
        }
    }
    Ok(())
}

fn validate_endpoint(endpoint: &str) -> AppResult<()> {
    let parsed = url::Url::parse(endpoint)
        .map_err(|_| std::io::Error::other("OLP_OTLP_TRACES_ENDPOINT must be a valid URL"))?;
    if !matches!(parsed.scheme(), "http" | "https") || parsed.host_str().is_none() {
        return Err(std::io::Error::other(
            "OLP_OTLP_TRACES_ENDPOINT must be an HTTP or HTTPS URL with a host",
        )
        .into());
    }
    if !parsed.username().is_empty() || parsed.password().is_some() || parsed.fragment().is_some() {
        return Err(std::io::Error::other(
            "OLP_OTLP_TRACES_ENDPOINT must not contain credentials or a fragment",
        )
        .into());
    }
    Ok(())
}

async fn load_headers(path: Option<&PathBuf>) -> AppResult<HashMap<String, String>> {
    let Some(path) = path else {
        return Ok(HashMap::new());
    };
    check_secret_permissions(path).await?;
    let contents = read_secret_file(path).await?;
    let values: HashMap<String, String> = serde_json::from_str(&contents).map_err(|_| {
        std::io::Error::other("OLP_OTLP_HEADERS_FILE must contain a JSON object of string values")
    })?;
    for (name, value) in &values {
        axum::http::HeaderName::from_bytes(name.as_bytes()).map_err(|_| {
            std::io::Error::other("OLP_OTLP_HEADERS_FILE contains an invalid header name")
        })?;
        axum::http::HeaderValue::from_str(value).map_err(|_| {
            std::io::Error::other("OLP_OTLP_HEADERS_FILE contains an invalid header value")
        })?;
    }
    Ok(values)
}

fn record_export_drops(count: u64) {
    let _ =
        TRACE_EXPORT_DROPPED_TOTAL.fetch_update(Ordering::Relaxed, Ordering::Relaxed, |current| {
            Some(current.saturating_add(count))
        });
}

#[must_use]
pub(crate) fn export_dropped_total() -> u64 {
    TRACE_EXPORT_DROPPED_TOTAL.load(Ordering::Relaxed)
}

#[cfg(test)]
mod tests {
    use super::{reject_ambient_otlp_headers, validate_endpoint};

    #[test]
    fn trace_endpoint_requires_secret_free_http_url() {
        for valid in [
            "http://collector:4318/v1/traces",
            "https://collector.example/v1/traces?tenant=one",
        ] {
            validate_endpoint(valid).unwrap();
        }
        for invalid in [
            "grpc://collector:4317",
            "https://",
            "https://user:secret@collector.example/v1/traces",
            "https://collector.example/v1/traces#secret",
        ] {
            let error = validate_endpoint(invalid).unwrap_err().to_string();
            assert!(!error.contains("user:secret"));
            assert!(!error.contains("#secret"));
        }
    }

    #[test]
    fn ambient_otlp_headers_are_rejected_without_exposing_values() {
        for blocked in [
            "OTEL_EXPORTER_OTLP_TRACES_HEADERS",
            "OTEL_EXPORTER_OTLP_HEADERS",
        ] {
            let error = reject_ambient_otlp_headers(|name| {
                (name == blocked).then(|| "authorization=secret".into())
            })
            .unwrap_err()
            .to_string();

            assert!(error.contains(blocked));
            assert!(error.contains("OLP_OTLP_HEADERS_FILE"));
            assert!(!error.contains("authorization=secret"));
        }
    }
}
