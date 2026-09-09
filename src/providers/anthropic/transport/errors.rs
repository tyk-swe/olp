use crate::inference::transport::TransportError;
use ::http::HeaderValue;
use ::http::StatusCode;

use crate::providers::anthropic::ApiKey;
use crate::providers::endpoint::Error;
use crate::providers::transport_common;
use crate::providers::transport_io::ProviderResponseIo;

pub(crate) const PROVIDER: &str = "Anthropic";
pub(crate) const RESPONSE_IO: ProviderResponseIo = ProviderResponseIo::new(PROVIDER);

pub(crate) fn secret_header(api_key: &ApiKey) -> Result<HeaderValue, TransportError> {
    transport_common::secret_header(api_key.expose(), PROVIDER)
}

pub(crate) fn safe_upstream_error_message(
    status: StatusCode,
    body: &[u8],
    api_key: &str,
) -> String {
    transport_common::safe_upstream_error_message(PROVIDER, status, body, api_key)
}

pub(crate) fn map_endpoint_error(error: Error) -> TransportError {
    let dns_timeout = matches!(error, Error::DnsTimeout { .. });
    transport_common::map_endpoint_error(error, dns_timeout)
}

pub(crate) fn map_send_error(error: reqwest::Error) -> TransportError {
    transport_common::map_send_error(PROVIDER, RESPONSE_IO, error)
}
