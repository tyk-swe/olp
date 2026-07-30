use std::{fmt, time::Duration};

use http::{HeaderValue, StatusCode};
use olp_domain::{AttemptFailureClass, MediaSpoolError, TransportError, TransportPhase};
use tokio::time::Instant;
use zeroize::Zeroizing;

use crate::{
    openai::{OpenAiApiKey, endpoint::EndpointError},
    transport_common,
    transport_io::ProviderResponseIo,
};

const PROVIDER: &str = "OpenAI";
const RESPONSE_IO: ProviderResponseIo = ProviderResponseIo::new(PROVIDER);

pub(super) use crate::{
    transport_common::{protocol_body_error, transport_error},
    transport_io::bounded_duration,
};

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
    transport_common::safe_upstream_error_message(PROVIDER, status, body, api_key)
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
    phase_deadline
        .min(attempt_deadline)
        .checked_duration_since(Instant::now())
}

pub(super) fn map_endpoint_error(error: EndpointError) -> TransportError {
    let dns_timeout = matches!(error, EndpointError::DnsTimeout);
    transport_common::map_endpoint_error(error, dns_timeout)
}

pub(super) fn map_send_error(error: reqwest::Error) -> TransportError {
    transport_common::map_send_error(PROVIDER, RESPONSE_IO, error)
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

pub(super) fn first_byte_timeout() -> TransportError {
    transport_error(
        TransportPhase::FirstByte,
        AttemptFailureClass::Timeout,
        false,
        "OpenAI first-byte deadline elapsed",
    )
}
