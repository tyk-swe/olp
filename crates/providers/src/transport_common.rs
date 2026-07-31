//! Shared request metadata and error construction for native HTTP transports.

use std::{
    collections::BTreeMap,
    fmt,
    sync::Mutex,
    time::{Duration, Instant, SystemTime},
};

use http::{HeaderMap, HeaderValue, StatusCode, header};
use olp_domain::{AttemptFailureClass, SourceExtensions, Surface, TransportError, TransportPhase};
use zeroize::Zeroizing;

use crate::transport_io::ProviderResponseIo;

const MAX_PROVIDER_RETRY_AFTER: Duration = Duration::from_secs(5 * 60);
const MAX_PROVIDER_RATE_LIMIT_MODELS: usize = 256;

#[derive(Default)]
pub(crate) struct RateLimitCooldown {
    state: Mutex<RateLimitCooldownState>,
}

#[derive(Default)]
struct RateLimitCooldownState {
    credential_until: Option<Instant>,
    until_by_model: BTreeMap<String, Instant>,
}

impl RateLimitCooldown {
    pub(crate) fn check(&self, upstream_model: &str) -> Result<(), TransportError> {
        self.check_scope(Some(upstream_model))
    }

    pub(crate) fn check_credential(&self) -> Result<(), TransportError> {
        self.check_scope(None)
    }

    fn check_scope(&self, upstream_model: Option<&str>) -> Result<(), TransportError> {
        let now = Instant::now();
        let mut state = self
            .state
            .lock()
            .expect("provider rate-limit cooldown lock poisoned");
        if state.credential_until.is_some_and(|until| until <= now) {
            state.credential_until = None;
        }
        let mut model_until =
            upstream_model.and_then(|model| state.until_by_model.get(model).copied());
        if model_until.is_some_and(|until| until <= now) {
            state
                .until_by_model
                .remove(upstream_model.expect("model cooldown came from a model"));
            model_until = None;
        }
        let Some(until) = state.credential_until.into_iter().chain(model_until).max() else {
            return Ok(());
        };
        let seconds = until.saturating_duration_since(now).as_secs().max(1);
        drop(state);
        Err(transport_error(
            TransportPhase::Connect,
            AttemptFailureClass::RateLimit,
            false,
            format!("provider rate-limit cooldown is active for another {seconds} seconds"),
        ))
    }

    pub(crate) fn observe(&self, upstream_model: Option<&str>, headers: &HeaderMap) {
        let Some(duration) = retry_after(headers, SystemTime::now()) else {
            return;
        };
        let now = Instant::now();
        let until = now + duration;
        let mut state = self
            .state
            .lock()
            .expect("provider rate-limit cooldown lock poisoned");
        state.until_by_model.retain(|_, until| *until > now);
        if let Some(model) = upstream_model
            && !state.until_by_model.contains_key(model)
            && state.until_by_model.len() >= MAX_PROVIDER_RATE_LIMIT_MODELS
            && let Some(oldest) = state
                .until_by_model
                .iter()
                .min_by_key(|(_, until)| *until)
                .map(|(model, _)| model.clone())
        {
            state.until_by_model.remove(&oldest);
        }
        let entry = match upstream_model {
            Some(model) => state
                .until_by_model
                .entry(model.to_owned())
                .or_insert(until),
            None => state.credential_until.get_or_insert(until),
        };
        *entry = (*entry).max(until);
    }
}

fn retry_after(headers: &HeaderMap, now: SystemTime) -> Option<Duration> {
    headers
        .get_all(header::RETRY_AFTER)
        .iter()
        .filter_map(|value| value.to_str().ok())
        .filter_map(|value| {
            let value = value.trim();
            value
                .parse::<u64>()
                .ok()
                .map(Duration::from_secs)
                .or_else(|| {
                    httpdate::parse_http_date(value)
                        .ok()?
                        .duration_since(now)
                        .ok()
                })
        })
        .filter(|duration| !duration.is_zero())
        .map(|duration| duration.min(MAX_PROVIDER_RETRY_AFTER))
        .max()
}

pub(crate) fn secret_header(
    secret: &str,
    provider: &'static str,
) -> Result<HeaderValue, TransportError> {
    let value = Zeroizing::new(secret.as_bytes().to_vec());
    HeaderValue::from_bytes(value.as_slice()).map_err(|_| {
        protocol_error(format!(
            "{provider} API key cannot be represented as a header"
        ))
    })
}

pub(crate) fn safe_upstream_error_message(
    provider: &'static str,
    status: StatusCode,
    body: &[u8],
    secret: &str,
) -> String {
    let message = serde_json::from_slice::<serde_json::Value>(body)
        .ok()
        .and_then(|value| value.get("error").cloned())
        .and_then(|error| {
            error
                .get("message")
                .and_then(serde_json::Value::as_str)
                .map(str::to_owned)
        })
        .map(|message| message.replace(secret, "[REDACTED]"))
        .map(|message| message.chars().take(512).collect::<String>());
    match message {
        Some(message) if !message.is_empty() => {
            format!("{provider} returned HTTP {status}: {message}")
        }
        _ => format!("{provider} returned HTTP {status}"),
    }
}

pub(crate) fn source_extensions(
    surface: Surface,
    values: BTreeMap<String, serde_json::Value>,
) -> SourceExtensions {
    let values = values
        .into_iter()
        .map(|(key, value)| {
            let escaped = key.replace('~', "~0").replace('/', "~1");
            (format!("/{escaped}"), value)
        })
        .collect();
    SourceExtensions::new(surface, values)
}

pub(crate) fn map_endpoint_error(error: impl fmt::Display, dns_timeout: bool) -> TransportError {
    let class = if dns_timeout {
        AttemptFailureClass::Timeout
    } else {
        AttemptFailureClass::Connect
    };
    transport_error(TransportPhase::Connect, class, false, error.to_string())
}

pub(crate) fn map_send_error(
    provider: &'static str,
    response_io: ProviderResponseIo,
    error: reqwest::Error,
) -> TransportError {
    if error.is_connect() {
        transport_error(
            TransportPhase::Connect,
            if error.is_timeout() {
                AttemptFailureClass::Timeout
            } else {
                AttemptFailureClass::Connect
            },
            false,
            format!("{provider} connection failed"),
        )
    } else if error.is_timeout() {
        response_io.first_byte_timeout()
    } else {
        transport_error(
            TransportPhase::FirstByte,
            AttemptFailureClass::Connect,
            false,
            format!("{provider} request failed before response headers"),
        )
    }
}

pub(crate) fn protocol_error(message: impl Into<String>) -> TransportError {
    transport_error(
        TransportPhase::Connect,
        AttemptFailureClass::Protocol,
        false,
        message,
    )
}

pub(crate) fn protocol_body_error(message: impl Into<String>) -> TransportError {
    transport_error(
        TransportPhase::Body,
        AttemptFailureClass::Protocol,
        false,
        message,
    )
}

pub(crate) fn transport_error(
    phase: TransportPhase,
    class: AttemptFailureClass,
    response_committed: bool,
    message: impl Into<String>,
) -> TransportError {
    TransportError {
        phase,
        class,
        response_committed,
        message: message.into(),
    }
}

pub(crate) const MAX_INLINE_MEDIA_BYTES: usize = 1024 * 1024;

/// Reads a bounded inline-media handle from the spool and returns its bytes
/// base64-encoded, shared by every connector that hydrates inline media.
pub(crate) async fn read_inline_media(
    marker: &str,
    spool: Option<&std::sync::Arc<dyn olp_domain::MediaSpool>>,
) -> Result<String, TransportError> {
    use base64::{Engine as _, engine::general_purpose::STANDARD};
    use futures::StreamExt as _;

    let handle = olp_domain::media_handle_from_inline_marker(marker)
        .ok_or_else(|| protocol_error("invalid bounded inline-media handle"))?;
    let spool = spool.ok_or_else(|| protocol_error("bounded inline-media spool is unavailable"))?;
    let opened = spool.open(&handle).await.map_err(|error| {
        protocol_error(format!(
            "bounded inline-media handle cannot be opened: {error}"
        ))
    })?;
    if opened
        .artifact
        .content_length
        .is_none_or(|length| length > MAX_INLINE_MEDIA_BYTES as u64)
    {
        return Err(protocol_error("bounded inline media exceeded its limit"));
    }
    let mut stream = opened.bytes;
    let mut bytes = Vec::new();
    while let Some(chunk) = stream.next().await {
        let chunk = chunk.map_err(|error| {
            protocol_error(format!("bounded inline-media read failed: {error}"))
        })?;
        if bytes.len().saturating_add(chunk.len()) > MAX_INLINE_MEDIA_BYTES {
            return Err(protocol_error("bounded inline media exceeded its limit"));
        }
        bytes.extend_from_slice(&chunk);
    }
    Ok(STANDARD.encode(bytes))
}

#[cfg(test)]
mod rate_limit_tests {
    use super::*;

    #[test]
    fn retry_after_supports_delta_and_http_date_with_a_safe_cap() {
        let now = SystemTime::UNIX_EPOCH + Duration::from_secs(1_000_000);
        let mut headers = HeaderMap::new();
        headers.insert(header::RETRY_AFTER, HeaderValue::from_static("999999"));
        assert_eq!(retry_after(&headers, now), Some(MAX_PROVIDER_RETRY_AFTER));

        headers.insert(
            header::RETRY_AFTER,
            HeaderValue::from_str(&httpdate::fmt_http_date(now + Duration::from_secs(45))).unwrap(),
        );
        assert_eq!(retry_after(&headers, now), Some(Duration::from_secs(45)));

        headers.insert(header::RETRY_AFTER, HeaderValue::from_static("invalid"));
        assert_eq!(retry_after(&headers, now), None);
    }

    #[test]
    fn cooldown_is_model_and_connector_scoped() {
        let first_credential = RateLimitCooldown::default();
        let second_credential = RateLimitCooldown::default();
        let mut headers = HeaderMap::new();
        headers.insert(header::RETRY_AFTER, HeaderValue::from_static("30"));

        first_credential.observe(Some("limited-model"), &headers);

        assert!(first_credential.check("limited-model").is_err());
        assert!(first_credential.check("other-model").is_ok());
        assert!(second_credential.check("limited-model").is_ok());

        first_credential.observe(None, &headers);
        assert!(first_credential.check("other-model").is_err());
        assert!(first_credential.check_credential().is_err());

        for index in 0..=MAX_PROVIDER_RATE_LIMIT_MODELS {
            second_credential.observe(Some(&format!("model-{index}")), &headers);
        }
        assert_eq!(
            second_credential
                .state
                .lock()
                .expect("provider rate-limit cooldown lock poisoned")
                .until_by_model
                .len(),
            MAX_PROVIDER_RATE_LIMIT_MODELS
        );
    }
}
