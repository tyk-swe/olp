use crate::domain::ports::TransportError;
use http::{HeaderValue, StatusCode};

use crate::providers::{
    gemini::{ApiKey, SecretBearerToken, endpoint::Error},
    transport_common,
    transport_io::ProviderResponseIo,
};

const PROVIDER: &str = "Gemini";
const RESPONSE_IO: ProviderResponseIo = ProviderResponseIo::new(PROVIDER);

pub(super) fn secret_header(api_key: &ApiKey) -> Result<HeaderValue, TransportError> {
    transport_common::secret_header(api_key.expose(), PROVIDER)
}

pub(super) fn bearer_header(token: &SecretBearerToken) -> Result<HeaderValue, TransportError> {
    transport_common::bearer_header(token.expose(), PROVIDER)
}

pub(super) fn safe_upstream_error_message(
    status: StatusCode,
    body: &[u8],
    api_key: &str,
) -> String {
    transport_common::safe_upstream_error_message(PROVIDER, status, body, api_key)
}

pub(super) fn map_endpoint_error(error: Error) -> TransportError {
    let dns_timeout = matches!(
        error,
        Error::Common(crate::providers::endpoint::Error::DnsTimeout { .. })
    );
    transport_common::map_endpoint_error(error, dns_timeout)
}

pub(super) fn map_send_error(error: reqwest::Error) -> TransportError {
    transport_common::map_send_error(PROVIDER, RESPONSE_IO, error)
}
