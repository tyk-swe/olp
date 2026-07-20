use std::{fmt, time::Duration};

use http::{HeaderValue, StatusCode};
use olp_domain::{AttemptFailureClass, MediaSpoolError, TransportError, TransportPhase};
use tokio::time::Instant;
use zeroize::Zeroizing;

use crate::openai::{OpenAiApiKey, endpoint::EndpointError};

pub(super) fn serialize_wire<T: serde::Serialize>(
    operation: &'static str,
    wire: &T,
) -> Result<Vec<u8>, TransportError> {
    serde_json::to_vec(wire).map_err(|error| protocol_encode_error(operation, error))
}

pub(super) fn parse_wire<T: serde::de::DeserializeOwned>(
    operation: &'static str,
    body: &[u8],
) -> Result<T, TransportError> {
    serde_json::from_slice(body).map_err(|error| protocol_decode_error(operation, error))
}

pub(super) fn protocol_encode_error(
    operation: &'static str,
    error: impl fmt::Display,
) -> TransportError {
    transport_error(
        TransportPhase::Connect,
        AttemptFailureClass::Protocol,
        false,
        format!("cannot encode OpenAI {operation} request: {error}"),
    )
}

pub(super) fn protocol_decode_error(
    operation: &'static str,
    error: impl fmt::Display,
) -> TransportError {
    transport_error(
        TransportPhase::Body,
        AttemptFailureClass::Protocol,
        false,
        format!("OpenAI {operation} response is invalid: {error}"),
    )
}

pub(super) fn protocol_body_error(message: impl Into<String>) -> TransportError {
    transport_error(
        TransportPhase::Body,
        AttemptFailureClass::Protocol,
        false,
        message,
    )
}

pub(super) fn map_spool_error(error: MediaSpoolError) -> TransportError {
    transport_error(
        TransportPhase::Body,
        AttemptFailureClass::Protocol,
        false,
        format!("bounded media spool failed: {error}"),
    )
}

pub(super) fn bearer_header(api_key: &OpenAiApiKey) -> Result<HeaderValue, TransportError> {
    let mut value = Zeroizing::new(Vec::with_capacity(7 + api_key.expose().len()));
    value.extend_from_slice(b"Bearer ");
    value.extend_from_slice(api_key.expose().as_bytes());
    HeaderValue::from_bytes(value.as_slice()).map_err(|_| {
        transport_error(
            TransportPhase::Connect,
            AttemptFailureClass::Protocol,
            false,
            "OpenAI API key cannot be represented as an HTTP header",
        )
    })
}

pub(super) fn raw_api_key_header(api_key: &OpenAiApiKey) -> Result<HeaderValue, TransportError> {
    HeaderValue::from_bytes(api_key.expose().as_bytes()).map_err(|_| {
        transport_error(
            TransportPhase::Connect,
            AttemptFailureClass::Protocol,
            false,
            "OpenAI API key cannot be represented as an HTTP header",
        )
    })
}

pub(super) fn safe_upstream_error_message(
    status: StatusCode,
    body: &[u8],
    api_key: &str,
) -> String {
    let message = serde_json::from_slice::<serde_json::Value>(body)
        .ok()
        .and_then(|value| value.get("error").cloned())
        .and_then(|error| {
            error
                .get("message")
                .and_then(|message| message.as_str())
                .map(str::to_owned)
        })
        .map(|message| message.replace(api_key, "[REDACTED]"))
        .map(|message| message.chars().take(512).collect::<String>());
    match message {
        Some(message) if !message.is_empty() => format!("OpenAI returned HTTP {status}: {message}"),
        _ => format!("OpenAI returned HTTP {status}"),
    }
}

pub(super) fn bounded_duration(configured: Duration, remaining: Duration) -> Duration {
    configured.min(remaining)
}

pub(super) fn bounded_instant(configured: Instant, deadline: Instant) -> Instant {
    configured.min(deadline)
}

pub(super) fn remaining(
    deadline: Instant,
    phase: TransportPhase,
) -> Result<Duration, TransportError> {
    deadline
        .checked_duration_since(Instant::now())
        .ok_or_else(|| {
            transport_error(
                phase,
                AttemptFailureClass::Timeout,
                false,
                "OpenAI attempt deadline elapsed",
            )
        })
}

pub(super) fn remaining_until(
    phase_deadline: Instant,
    attempt_deadline: Instant,
) -> Option<Duration> {
    bounded_instant(phase_deadline, attempt_deadline).checked_duration_since(Instant::now())
}

pub(super) fn map_endpoint_error(error: EndpointError) -> TransportError {
    let class = if matches!(error, EndpointError::DnsTimeout) {
        AttemptFailureClass::Timeout
    } else {
        AttemptFailureClass::Connect
    };
    transport_error(TransportPhase::Connect, class, false, error.to_string())
}

pub(super) fn map_send_error(error: reqwest::Error) -> TransportError {
    let (phase, class, message) = if error.is_connect() {
        (
            TransportPhase::Connect,
            if error.is_timeout() {
                AttemptFailureClass::Timeout
            } else {
                AttemptFailureClass::Connect
            },
            "OpenAI connection failed",
        )
    } else if error.is_timeout() {
        (
            TransportPhase::FirstByte,
            AttemptFailureClass::Timeout,
            "OpenAI first-byte deadline elapsed",
        )
    } else {
        (
            TransportPhase::FirstByte,
            AttemptFailureClass::Connect,
            "OpenAI request failed before response headers",
        )
    };
    transport_error(phase, class, false, message)
}

pub(super) fn map_ambiguous_send_error(error: reqwest::Error) -> TransportError {
    if error.is_connect() {
        return map_send_error(error);
    }
    transport_error(
        TransportPhase::Body,
        AttemptFailureClass::Ambiguous,
        true,
        "OpenAI multipart request may have been committed before transport failure",
    )
}

pub(super) fn ambiguous_multipart_timeout() -> TransportError {
    transport_error(
        TransportPhase::Body,
        AttemptFailureClass::Ambiguous,
        true,
        "OpenAI multipart request may have been committed before its first-byte deadline",
    )
}

pub(super) fn map_first_body_error(error: reqwest::Error) -> TransportError {
    transport_error(
        TransportPhase::FirstByte,
        if error.is_timeout() {
            AttemptFailureClass::Timeout
        } else {
            AttemptFailureClass::Connect
        },
        false,
        "OpenAI response body failed before its first byte",
    )
}

pub(super) fn map_body_error(error: reqwest::Error, committed: bool) -> TransportError {
    transport_error(
        TransportPhase::Body,
        if error.is_timeout() {
            AttemptFailureClass::Timeout
        } else {
            AttemptFailureClass::Connect
        },
        committed,
        "OpenAI response body failed",
    )
}

pub(super) fn first_byte_timeout() -> TransportError {
    transport_error(
        TransportPhase::FirstByte,
        AttemptFailureClass::Timeout,
        false,
        "OpenAI first-byte deadline elapsed",
    )
}

pub(super) fn body_idle_timeout() -> TransportError {
    transport_error(
        TransportPhase::Body,
        AttemptFailureClass::Timeout,
        false,
        "OpenAI response idle deadline elapsed",
    )
}

pub(super) fn attempt_body_timeout() -> TransportError {
    transport_error(
        TransportPhase::Body,
        AttemptFailureClass::Timeout,
        false,
        "OpenAI attempt deadline elapsed while reading the response",
    )
}

pub(super) fn transport_error(
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
