use http::{HeaderValue, StatusCode};
use olp_domain::TransportError;
use zeroize::Zeroizing;

use crate::{
    gemini::{GeminiApiKey, SecretBearerToken, endpoint::EndpointError},
    transport_common,
    transport_io::ProviderResponseIo,
};

const PROVIDER: &str = "Gemini";
const RESPONSE_IO: ProviderResponseIo = ProviderResponseIo::new(PROVIDER);

pub(super) use crate::transport_common::{
    protocol_body_error, protocol_error, source_extensions, transport_error,
};

pub(super) fn secret_header(api_key: &GeminiApiKey) -> Result<HeaderValue, TransportError> {
    transport_common::secret_header(api_key.expose(), PROVIDER)
}

pub(super) fn bearer_header(token: &SecretBearerToken) -> Result<HeaderValue, TransportError> {
    let mut value = Zeroizing::new(Vec::with_capacity(7 + token.expose().len()));
    value.extend_from_slice(b"Bearer ");
    value.extend_from_slice(token.expose().as_bytes());
    HeaderValue::from_bytes(value.as_slice())
        .map_err(|_| protocol_error("Google OAuth token cannot be represented as a header"))
}

pub(super) fn safe_upstream_error_message(
    status: StatusCode,
    body: &[u8],
    api_key: &str,
) -> String {
    transport_common::safe_upstream_error_message(PROVIDER, status, body, api_key)
}

pub(super) fn map_endpoint_error(error: EndpointError) -> TransportError {
    let dns_timeout = matches!(
        error,
        EndpointError::Common(crate::CommonEndpointError::DnsTimeout { .. })
    );
    transport_common::map_endpoint_error(error, dns_timeout)
}

pub(super) fn map_send_error(error: reqwest::Error) -> TransportError {
    transport_common::map_send_error(PROVIDER, RESPONSE_IO, error)
}
