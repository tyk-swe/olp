use crate::domain::ports::TransportError;
use http::{HeaderValue, StatusCode};

use crate::providers::{
    anthropic::ApiKey, endpoint::Error, transport_common, transport_io::ProviderResponseIo,
};

const PROVIDER: &str = "Anthropic";
const RESPONSE_IO: ProviderResponseIo = ProviderResponseIo::new(PROVIDER);

pub(super) fn secret_header(api_key: &ApiKey) -> Result<HeaderValue, TransportError> {
    transport_common::secret_header(api_key.expose(), PROVIDER)
}

pub(super) fn safe_upstream_error_message(
    status: StatusCode,
    body: &[u8],
    api_key: &str,
) -> String {
    transport_common::safe_upstream_error_message(PROVIDER, status, body, api_key)
}

pub(super) fn map_endpoint_error(error: Error) -> TransportError {
    let dns_timeout = matches!(error, Error::DnsTimeout { .. });
    transport_common::map_endpoint_error(error, dns_timeout)
}

pub(super) fn map_send_error(error: reqwest::Error) -> TransportError {
    transport_common::map_send_error(PROVIDER, RESPONSE_IO, error)
}
