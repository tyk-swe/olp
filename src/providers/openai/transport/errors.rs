use std::fmt;
use std::time::Duration;

use crate::inference::transport::AttemptFailureClass;
use crate::inference::transport::MediaSpoolError;
use crate::inference::transport::TransportError;
use crate::inference::transport::TransportPhase;
use ::http::HeaderValue;
use ::http::StatusCode;
use tokio::time::Instant;

use crate::providers::openai::ApiKey;
use crate::providers::openai::endpoint::Error;
use crate::providers::transport_common;
use crate::providers::transport_common::transport_error;
use crate::providers::transport_io::ProviderResponseIo;

const PROVIDER: &str = "OpenAI";
const RESPONSE_IO: ProviderResponseIo = ProviderResponseIo::new(PROVIDER);

pub(crate) fn serialize_wire<T: serde::Serialize>(
    operation: &'static str,
    wire: &T,
) -> Result<Vec<u8>, TransportError> {
    serde_json::to_vec(wire).map_err(|error| protocol_encode_error(operation, error))
}

pub(crate) fn parse_wire<T: serde::de::DeserializeOwned>(
    operation: &'static str,
    body: &[u8],
) -> Result<T, TransportError> {
    serde_json::from_slice(body).map_err(|error| protocol_decode_error(operation, error))
}

pub(crate) fn protocol_encode_error(
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

pub(crate) fn protocol_decode_error(
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

pub(crate) fn map_spool_error(error: MediaSpoolError) -> TransportError {
    transport_error(
        TransportPhase::Body,
        AttemptFailureClass::Protocol,
        false,
        format!("bounded media spool failed: {error}"),
    )
}

pub(crate) fn bearer_header(api_key: &ApiKey) -> Result<HeaderValue, TransportError> {
    transport_common::bearer_header(api_key.expose(), PROVIDER)
}

pub(crate) fn raw_api_key_header(api_key: &ApiKey) -> Result<HeaderValue, TransportError> {
    HeaderValue::from_bytes(api_key.expose().as_bytes()).map_err(|_| {
        transport_error(
            TransportPhase::Connect,
            AttemptFailureClass::Protocol,
            false,
            "OpenAI API key cannot be represented as an HTTP header",
        )
    })
}

pub(crate) fn safe_upstream_error_message(
    status: StatusCode,
    body: &[u8],
    api_key: &str,
) -> String {
    transport_common::safe_upstream_error_message(PROVIDER, status, body, api_key)
}

pub(crate) fn remaining(
    deadline: Instant,
    phase: TransportPhase,
) -> Result<Duration, TransportError> {
    RESPONSE_IO.remaining(deadline, phase)
}

pub(crate) fn remaining_until(
    phase_deadline: Instant,
    attempt_deadline: Instant,
) -> Option<Duration> {
    RESPONSE_IO.remaining_until(phase_deadline, attempt_deadline)
}

pub(crate) fn map_endpoint_error(error: Error) -> TransportError {
    let dns_timeout = matches!(
        error,
        Error::Common(crate::providers::endpoint::Error::DnsTimeout { .. })
    );
    transport_common::map_endpoint_error(error, dns_timeout)
}

pub(crate) fn map_send_error(error: reqwest::Error) -> TransportError {
    transport_common::map_send_error(PROVIDER, RESPONSE_IO, error)
}

pub(crate) fn map_ambiguous_send_error(error: reqwest::Error) -> TransportError {
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

pub(crate) fn ambiguous_multipart_timeout() -> TransportError {
    transport_error(
        TransportPhase::Body,
        AttemptFailureClass::Ambiguous,
        true,
        "OpenAI multipart request may have been committed before its first-byte deadline",
    )
}

pub(crate) fn first_byte_timeout() -> TransportError {
    RESPONSE_IO.first_byte_timeout()
}
