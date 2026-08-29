//! Optional cross-origin access to the inference gateway. Management stays
//! same-origin: it authenticates with cookies and enforces `Origin` itself.

use std::{str::FromStr, sync::Arc, time::Duration};

use axum::{
    body::Body,
    http::{HeaderValue, Method, Request},
    middleware,
    response::Response,
};
use thiserror::Error;
use tower::{Layer as _, ServiceExt as _};
use tower_http::cors::{AllowHeaders, AllowOrigin, CorsLayer};
use url::Url;

/// Comma-separated browser origins allowed to call the gateway. An empty value
/// keeps CORS disabled; `*` is never accepted.
#[derive(Clone, Debug, Default)]
pub struct CorsAllowedOrigins(pub Vec<HeaderValue>);

#[derive(Clone, Debug, Error, Eq, PartialEq)]
pub enum CorsOriginParseError {
    #[error("CORS origin `{0}` is not an absolute http(s) URL")]
    InvalidUrl(String),
    #[error(
        "CORS origin `{0}` must be scheme://host[:port] with no path, query, fragment, or userinfo"
    )]
    NotAnOrigin(String),
    #[error("CORS wildcard origins are not accepted; list each origin explicitly")]
    Wildcard,
}

impl FromStr for CorsAllowedOrigins {
    type Err = CorsOriginParseError;

    fn from_str(value: &str) -> Result<Self, Self::Err> {
        value
            .split(',')
            .map(str::trim)
            .filter(|part| !part.is_empty())
            .map(parse_origin)
            .collect::<Result<Vec<_>, _>>()
            .map(Self)
    }
}

fn parse_origin(value: &str) -> Result<HeaderValue, CorsOriginParseError> {
    if value == "*" {
        return Err(CorsOriginParseError::Wildcard);
    }
    let url = Url::parse(value).map_err(|_| CorsOriginParseError::InvalidUrl(value.to_owned()))?;
    let is_origin = matches!(url.scheme(), "http" | "https")
        && url.host_str().is_some()
        && url.username().is_empty()
        && url.password().is_none()
        && matches!(url.path(), "" | "/")
        && url.query().is_none()
        && url.fragment().is_none()
        && !value.ends_with('/');
    if !is_origin {
        return Err(CorsOriginParseError::NotAnOrigin(value.to_owned()));
    }
    HeaderValue::from_str(&url.origin().ascii_serialization())
        .map_err(|_| CorsOriginParseError::InvalidUrl(value.to_owned()))
}

pub(crate) fn gateway_cors_layer(origins: &[HeaderValue]) -> CorsLayer {
    CorsLayer::new()
        .allow_origin(AllowOrigin::list(origins.iter().cloned()))
        .allow_methods([Method::GET, Method::POST, Method::DELETE, Method::OPTIONS])
        .allow_headers(AllowHeaders::mirror_request())
        .allow_credentials(false)
        .max_age(Duration::from_secs(600))
}

pub(crate) async fn apply_gateway_cors(
    origins: Arc<[HeaderValue]>,
    request: Request<Body>,
    next: middleware::Next,
) -> Response {
    if !is_gateway_path(request.uri().path()) {
        return next.run(request).await;
    }
    gateway_cors_layer(&origins)
        .layer(next)
        .oneshot(request)
        .await
        .expect("gateway CORS service is infallible")
}

fn is_gateway_path(path: &str) -> bool {
    ["/openai/", "/v1/", "/anthropic/", "/gemini/"]
        .iter()
        .any(|prefix| path.starts_with(prefix))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn origins_parse_to_canonical_serializations() {
        let origins = "https://App.Example.com, http://localhost:5173,,"
            .parse::<CorsAllowedOrigins>()
            .unwrap();
        assert_eq!(
            origins.0,
            vec![
                HeaderValue::from_static("https://app.example.com"),
                HeaderValue::from_static("http://localhost:5173"),
            ]
        );
        assert!("".parse::<CorsAllowedOrigins>().unwrap().0.is_empty());
    }

    #[test]
    fn non_origins_and_wildcards_are_rejected() {
        assert_eq!(
            "*".parse::<CorsAllowedOrigins>().unwrap_err(),
            CorsOriginParseError::Wildcard
        );
        for value in [
            "https://app.example.com/path",
            "https://app.example.com/",
            "https://app.example.com?x=1",
            "https://user:pw@app.example.com",
            "ftp://app.example.com",
            "app.example.com",
        ] {
            assert!(value.parse::<CorsAllowedOrigins>().is_err(), "{value}");
        }
    }
}
