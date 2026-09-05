use std::{
    pin::Pin,
    task::{Context, Poll},
    time::Duration,
};

use axum::{
    body::{Body, HttpBody},
    http::HeaderMap,
};
use olp_engine::domain::{
    auth::{ApiKeyStatus, GatewayCapability},
    canonical::identity::Surface,
    ids::{ApiKeyLookupId, RouteSlug},
};
use olp_engine::inference::{
    limits::{LimitDimension, LimitError, LimitRequest, Reservation, outage_reservation},
    principal::Principal,
};
use olp_engine::protocols::openai::embeddings::EmbeddingWireInput;
use serde::Deserialize;

use crate::{
    gateway::{self, endpoint_policy::classification::TokenEstimate},
    public_http::state::RequestBoundaryState,
};

const LITELLM_API_KEY_HEADER: &str = "x-litellm-api-key";

pub(crate) struct ReleaseReservationBody {
    pub(crate) inner: Body,
    pub(crate) reservation: Reservation,
}

impl HttpBody for ReleaseReservationBody {
    type Data = bytes::Bytes;
    type Error = axum::Error;

    fn poll_frame(
        self: Pin<&mut Self>,
        context: &mut Context<'_>,
    ) -> Poll<Option<Result<http_body::Frame<Self::Data>, Self::Error>>> {
        let this = self.get_mut();
        let poll = Pin::new(&mut this.inner).poll_frame(context);
        if matches!(poll, Poll::Ready(None)) {
            this.reservation.spawn_release();
        }
        poll
    }

    fn is_end_stream(&self) -> bool {
        self.inner.is_end_stream()
    }

    fn size_hint(&self) -> http_body::SizeHint {
        self.inner.size_hint()
    }
}

impl Drop for ReleaseReservationBody {
    fn drop(&mut self) {
        self.reservation.spawn_release();
    }
}

pub(super) fn authenticate_inference_headers(
    state: &RequestBoundaryState,
    headers: &HeaderMap,
    surface: Surface,
    gateway_capability: Option<GatewayCapability>,
) -> Result<Principal, crate::public_http::problem::Problem> {
    let litellm_header_present = headers.contains_key(LITELLM_API_KEY_HEADER);
    let native_token = native_inference_token(headers, surface);
    let litellm_token = if litellm_header_present {
        let mut values = headers.get_all(LITELLM_API_KEY_HEADER).iter();
        match (values.next(), values.next()) {
            (Some(value), None) => litellm_header_token(value),
            _ => None,
        }
    } else {
        None
    };
    let token = if litellm_header_present {
        litellm_token.ok_or_else(|| {
            crate::public_http::problem::Problem::unauthorized(
                "The API key is invalid or unavailable.",
            )
        })?
    } else {
        native_token.ok_or_else(|| {
            crate::public_http::problem::Problem::unauthorized(
                "The API key is invalid or unavailable.",
            )
        })?
    };
    let auth_hmac_key = &state.auth_hmac_key;
    let lookup = auth_hmac_key
        .lookup_id(token)
        .map_err(|_| {
            crate::public_http::problem::Problem::unauthorized(
                "The API key is invalid or unavailable.",
            )
        })?
        .to_owned();
    let lookup_id = ApiKeyLookupId::parse(&lookup).map_err(|_| {
        crate::public_http::problem::Problem::unauthorized("The API key is invalid or unavailable.")
    })?;
    let snapshot = state.inference.runtime().pin();
    let key = snapshot.api_keys.get(&lookup_id).ok_or_else(|| {
        crate::public_http::problem::Problem::unauthorized("The API key is invalid or unavailable.")
    })?;
    auth_hmac_key
        .parse_and_verify(token, key.digest.as_bytes())
        .map_err(|_| {
            crate::public_http::problem::Problem::unauthorized(
                "The API key is invalid or unavailable.",
            )
        })?;
    if key.status != ApiKeyStatus::Active
        || key
            .expires_at
            .is_some_and(|expires_at| expires_at <= chrono::Utc::now())
    {
        return Err(crate::public_http::problem::Problem::unauthorized(
            "The API key is invalid or unavailable.",
        ));
    }
    if litellm_header_present
        && native_token.is_some_and(|native_token| {
            native_token != token && auth_hmac_key.lookup_id(native_token).is_ok()
        })
    {
        return Err(crate::public_http::problem::Problem::unauthorized(
            "The API key is invalid or unavailable.",
        ));
    }
    Ok(Principal::new(
        snapshot,
        lookup_id,
        surface,
        gateway_capability,
    ))
}

const MAX_CONCURRENCY_RETRY_HINT: Duration = Duration::from_secs(5);

/// A concurrency slot frees the moment any in-flight request finishes, which is
/// nothing like a lease TTL. Handing back the oldest live lease's expiry told a
/// client to idle for the longest route's timeout — minutes, potentially, and
/// for requests to fast routes — over a slot usually microseconds away.
fn retry_hint(dimension: LimitDimension, retry_after: Duration) -> Duration {
    if dimension == LimitDimension::Concurrency {
        retry_after.min(MAX_CONCURRENCY_RETRY_HINT)
    } else {
        retry_after
    }
}

/// Sizes the crash-recovery lease from the route actually being called. Only
/// when the route is still unknown does the widest route the key may use apply.
fn reservation_route_timeout(
    routes: &std::collections::BTreeMap<RouteSlug, olp_engine::domain::routing::route::Route>,
    allowed_routes: &std::collections::BTreeSet<RouteSlug>,
    route: Option<&RouteSlug>,
) -> Duration {
    route.and_then(|slug| routes.get(slug)).map_or_else(
        || {
            routes
                .iter()
                .filter(|(slug, _)| allowed_routes.is_empty() || allowed_routes.contains(*slug))
                .map(|(_, route)| route.overall_timeout.as_duration())
                .max()
                .unwrap_or(DEFAULT_RESERVATION_LEASE_TIMEOUT)
        },
        |route| route.overall_timeout.as_duration(),
    )
}

/// Lease TTL when the key can reach no route at all — only reachable for a
/// request the router is about to reject anyway.
const DEFAULT_RESERVATION_LEASE_TIMEOUT: Duration = Duration::from_secs(30);

pub(super) async fn reserve_http_inference_limits(
    state: &RequestBoundaryState,
    principal: &Principal,
    route: Option<&olp_engine::domain::ids::RouteSlug>,
    requested_tokens: i64,
) -> Result<Option<Reservation>, gateway::error::InferenceError> {
    if !principal.key().limits.has_hard_limits() {
        return Ok(None);
    }
    let limiter = state.inference.limiter();
    let cost_budget_configured = principal.key().limits.daily_cost_limit.is_some()
        || principal.key().limits.monthly_cost_limit.is_some();
    let Some(backend) = limiter.current() else {
        return outage_reservation(limiter, "backend_missing", cost_budget_configured)
            .map_err(Into::into);
    };
    let tokens_per_minute = principal
        .key()
        .limits
        .tokens_per_minute
        .map(|value| i64::try_from(value.get()))
        .transpose()
        .map_err(|_| gateway::error::InferenceError::unavailable("limit_configuration_invalid"))?;
    if let Some(limit) = tokens_per_minute
        && requested_tokens > limit
    {
        return Err(
            olp_engine::inference::error::Error::request_exceeds_token_limit(
                requested_tokens,
                limit,
            )
            .into(),
        );
    }
    let route_timeout = reservation_route_timeout(
        &principal.runtime().routes,
        &principal.key().allowed_routes,
        route,
    );
    let Ok(result) = tokio::time::timeout(
        Duration::from_secs(1),
        backend.reserve(LimitRequest {
            api_key_id: principal.key().id.as_uuid(),
            lookup_id: principal.lookup_id().as_str(),
            requests_per_minute: principal
                .key()
                .limits
                .requests_per_minute
                .map(|value| i64::from(value.get())),
            tokens_per_minute,
            max_concurrency: principal
                .key()
                .limits
                .concurrency
                .map(|value| i64::from(value.get())),
            daily_cost_limit: principal.key().limits.daily_cost_limit,
            monthly_cost_limit: principal.key().limits.monthly_cost_limit,
            requested_tokens,
            // Account for the bounded body-read phase in addition to the
            // route deadline. This is only a crash-recovery backstop.
            lease_ttl: route_timeout.saturating_add(Duration::from_secs(60)),
        }),
    )
    .await
    else {
        return outage_reservation(limiter, "timeout", cost_budget_configured).map_err(Into::into);
    };
    match result {
        Ok(lease) => Ok(Some(Reservation::distributed(lease))),
        Err(LimitError::Exceeded {
            dimension,
            retry_after,
        }) => {
            limiter.record_rejection(dimension);
            Err(gateway::error::InferenceError::rate_limited(
                dimension,
                retry_hint(dimension, retry_after),
            ))
        }
        Err(error) => outage_reservation(limiter, &error.to_string(), cost_budget_configured)
            .map_err(Into::into),
    }
}

pub(crate) fn estimate_http_json_request_tokens(category: TokenEstimate, body: &[u8]) -> i64 {
    let encoded_body = body.len().saturating_add(3) / 4;
    let baseline = if category == TokenEstimate::Generation {
        let value = serde_json::from_slice::<serde_json::Value>(body).ok();
        let output = value
            .as_ref()
            .and_then(|value| {
                [
                    "/max_completion_tokens",
                    "/max_tokens",
                    "/max_output_tokens",
                    "/generationConfig/maxOutputTokens",
                ]
                .into_iter()
                .find_map(|pointer| value.pointer(pointer).and_then(serde_json::Value::as_u64))
            })
            .and_then(|value| usize::try_from(value).ok())
            .unwrap_or(4_096)
            .max(1);
        let candidates = value
            .as_ref()
            .and_then(|value| {
                value
                    .pointer("/n")
                    .or_else(|| value.pointer("/generationConfig/candidateCount"))
                    .and_then(serde_json::Value::as_u64)
            })
            .and_then(|value| usize::try_from(value).ok())
            .unwrap_or(1)
            .max(1);
        output.saturating_mul(candidates)
    } else {
        1
    };
    let byte_estimate = encoded_body.saturating_add(baseline).max(1);
    // Generation paths and embeddings are mutually exclusive, so the body is
    // parsed at most once per request, and only for paths that consume it.
    let embedding_token_floor = if category == TokenEstimate::Embeddings {
        serde_json::from_slice::<EmbeddingTokenProbe>(body)
            .ok()
            .map(|probe| embedding_token_count(probe.input))
            .unwrap_or(0)
    } else {
        0
    };
    i64::try_from(byte_estimate.max(embedding_token_floor)).unwrap_or(i64::MAX)
}

/// Probes only the `input` field of an embeddings request to estimate its
/// token floor, reusing the canonical wire shape so token-array variants stay
/// in sync with the request codec.
#[derive(Deserialize)]
struct EmbeddingTokenProbe {
    input: EmbeddingWireInput,
}

fn embedding_token_count(input: EmbeddingWireInput) -> usize {
    match input {
        EmbeddingWireInput::Text(_) | EmbeddingWireInput::Texts(_) => 0,
        EmbeddingWireInput::Tokens(tokens) => tokens.len(),
        EmbeddingWireInput::TokenArrays(arrays) => arrays.iter().map(Vec::len).sum(),
    }
}

pub(super) const fn estimate_http_non_json_request_tokens(category: TokenEstimate) -> i64 {
    match category {
        TokenEstimate::Generation => 4_096,
        TokenEstimate::Transcription => 1_500,
        TokenEstimate::Media => 2_000,
        TokenEstimate::Default | TokenEstimate::Embeddings => 1,
    }
}

fn native_inference_token(headers: &HeaderMap, surface: Surface) -> Option<&str> {
    match surface {
        Surface::OpenAi => headers
            .get(axum::http::header::AUTHORIZATION)
            .and_then(|value| value.to_str().ok())
            .and_then(bearer_token),
        Surface::Anthropic => inference_header_token(headers, "x-api-key"),
        Surface::Gemini => inference_header_token(headers, "x-goog-api-key"),
    }
}

fn litellm_header_token(value: &axum::http::HeaderValue) -> Option<&str> {
    let value = value.to_str().ok()?;
    bearer_token(value).or_else(|| non_whitespace_token(value))
}

fn bearer_token(value: &str) -> Option<&str> {
    let (scheme, token) = value.split_once(' ')?;
    (scheme.eq_ignore_ascii_case("bearer") && non_whitespace_token(token).is_some())
        .then_some(token)
}

fn non_whitespace_token(value: &str) -> Option<&str> {
    (!value.is_empty() && !value.contains(char::is_whitespace)).then_some(value)
}

fn inference_header_token<'a>(headers: &'a HeaderMap, name: &'static str) -> Option<&'a str> {
    headers
        .get(name)
        .and_then(|value| value.to_str().ok())
        .and_then(non_whitespace_token)
}

#[cfg(test)]
mod tests {
    use axum::http::{HeaderValue, header};

    use super::*;

    #[test]
    fn native_auth_headers_are_surface_specific_and_strictly_single_token() {
        let mut headers = HeaderMap::new();
        headers.insert(
            header::AUTHORIZATION,
            HeaderValue::from_static("bEaReR openai-key"),
        );
        headers.insert("x-api-key", HeaderValue::from_static("anthropic-key"));
        headers.insert("x-goog-api-key", HeaderValue::from_static("gemini-key"));
        assert_eq!(
            native_inference_token(&headers, Surface::OpenAi),
            Some("openai-key")
        );
        assert_eq!(
            native_inference_token(&headers, Surface::Anthropic),
            Some("anthropic-key")
        );
        assert_eq!(
            native_inference_token(&headers, Surface::Gemini),
            Some("gemini-key")
        );

        for value in [
            "Bearer",
            "Bearer ",
            "Bearer two words",
            "Basic token",
            "token",
        ] {
            headers.insert(header::AUTHORIZATION, HeaderValue::from_str(value).unwrap());
            assert_eq!(
                native_inference_token(&headers, Surface::OpenAi),
                None,
                "{value:?}"
            );
        }
    }

    #[test]
    fn litellm_compatibility_header_accepts_raw_or_bearer_but_not_whitespace() {
        for (value, expected) in [
            ("raw-key", Some("raw-key")),
            ("Bearer wrapped-key", Some("wrapped-key")),
            ("bearer wrapped-key", Some("wrapped-key")),
            ("Bearer two words", None),
            (" key", None),
            ("", None),
        ] {
            assert_eq!(
                litellm_header_token(&HeaderValue::from_str(value).unwrap()),
                expected,
                "{value:?}"
            );
        }
    }

    #[test]
    fn non_json_estimates_are_conservative_by_operation_cost() {
        for (category, expected) in [
            (TokenEstimate::Generation, 4_096),
            (TokenEstimate::Transcription, 1_500),
            (TokenEstimate::Media, 2_000),
            (TokenEstimate::Default, 1),
            (TokenEstimate::Embeddings, 1),
        ] {
            assert_eq!(estimate_http_non_json_request_tokens(category), expected);
        }
    }

    use std::collections::{BTreeMap, BTreeSet};
    use std::num::{NonZeroU16, NonZeroU32};

    use olp_engine::domain::{
        ids::{DurationMs, ProviderId, RouteId, TargetId},
        routing::route::{Route, Target},
    };

    fn route(slug: &str, overall_timeout: Duration) -> (RouteSlug, Route) {
        let slug = RouteSlug::parse(slug).unwrap();
        let route = Route {
            id: RouteId::new(),
            routing_id: None,
            slug: slug.clone(),
            operations: std::collections::BTreeSet::new(),
            overall_timeout: DurationMs::new(
                u64::try_from(overall_timeout.as_millis()).unwrap_or(u64::MAX),
            ),
            max_attempts: NonZeroU16::new(1).unwrap(),
            targets: vec![Target {
                id: TargetId::new(),
                routing_id: None,
                provider_id: ProviderId::new(),
                upstream_model: "model".to_owned(),
                priority: 0,
                weight: NonZeroU32::new(1).unwrap(),
                timeout: DurationMs::new(1_000),
            }],
        };
        (slug, route)
    }

    #[test]
    fn a_concurrency_rejection_does_not_hand_back_a_lease_sized_retry_hint() {
        // The limiter reports the oldest live lease's expiry, which is sized by
        // the widest route's timeout plus a minute. A slot usually frees in
        // microseconds, so a conforming client would idle for nothing.
        assert_eq!(
            retry_hint(LimitDimension::Concurrency, Duration::from_secs(660)),
            MAX_CONCURRENCY_RETRY_HINT
        );
        assert_eq!(
            retry_hint(LimitDimension::Concurrency, Duration::from_millis(250)),
            Duration::from_millis(250)
        );
        // Fixed-window dimensions really do reset at the stated time.
        for dimension in [LimitDimension::Requests, LimitDimension::Tokens] {
            assert_eq!(
                retry_hint(dimension, Duration::from_secs(47)),
                Duration::from_secs(47)
            );
        }
    }

    #[test]
    fn the_lease_is_sized_from_the_route_being_called() {
        let routes = BTreeMap::from([
            route("fast", Duration::from_secs(5)),
            route("slow", Duration::from_secs(600)),
        ]);
        let fast = RouteSlug::parse("fast").unwrap();
        let unrestricted = BTreeSet::new();

        assert_eq!(
            reservation_route_timeout(&routes, &unrestricted, Some(&fast)),
            Duration::from_secs(5),
            "one slow route configured elsewhere must not stretch a fast route's lease"
        );
        // With no route yet identified the widest the key may reach still applies.
        assert_eq!(
            reservation_route_timeout(&routes, &unrestricted, None),
            Duration::from_secs(600)
        );
        assert_eq!(
            reservation_route_timeout(&routes, &BTreeSet::from([fast.clone()]), None),
            Duration::from_secs(5)
        );
        assert_eq!(
            reservation_route_timeout(&BTreeMap::new(), &unrestricted, None),
            DEFAULT_RESERVATION_LEASE_TIMEOUT
        );
        // A route the runtime does not know falls back the same way.
        let missing = RouteSlug::parse("missing").unwrap();
        assert_eq!(
            reservation_route_timeout(&routes, &unrestricted, Some(&missing)),
            Duration::from_secs(600)
        );
    }
}
