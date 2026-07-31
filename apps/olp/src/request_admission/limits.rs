use std::{
    future::Future,
    pin::Pin,
    sync::{Arc, Mutex},
    task::{Context, Poll},
    time::Duration,
};

use axum::{
    body::{Body, HttpBody},
    http::HeaderMap,
};
use olp_domain::{ApiKey, ApiKeyLookupId, ApiKeyStatus, MAX_ROUTE_TIMEOUT_MS, RouteSlug, Surface};
use olp_protocols::openai::EmbeddingWireInput;
use olp_storage::{DistributedLimiter, LimitError, LimitLease, LimitRequest};
use serde::Deserialize;

use crate::{GatewayState, RuntimeBundle, gateway, router::REQUEST_BODY_TIMEOUT};

type ReleaseFuture = Pin<Box<dyn Future<Output = ()> + Send + 'static>>;

struct InferenceReservationInner {
    release: Mutex<Option<ReleaseFuture>>,
    reconcile: Mutex<Option<DistributedReconciliation>>,
}

struct DistributedReconciliation {
    limiter: Arc<DistributedLimiter>,
    lease: LimitLease,
    store: olp_storage::PgStore,
    api_key_id: uuid::Uuid,
}

#[derive(Clone)]
pub(crate) struct InferenceReservation {
    inner: Arc<InferenceReservationInner>,
}

impl InferenceReservation {
    fn distributed(
        limiter: Arc<DistributedLimiter>,
        lease: LimitLease,
        store: olp_storage::PgStore,
        api_key_id: uuid::Uuid,
    ) -> Self {
        let reconcile = DistributedReconciliation {
            limiter: Arc::clone(&limiter),
            lease: lease.clone(),
            store: store.clone(),
            api_key_id,
        };
        Self {
            inner: Arc::new(InferenceReservationInner {
                release: Mutex::new(Some(Box::pin(async move {
                    gateway::limits::release_concurrency_in_background(
                        limiter,
                        lease,
                        Some((store, api_key_id)),
                    );
                }))),
                reconcile: Mutex::new(Some(reconcile)),
            }),
        }
    }

    #[cfg(test)]
    pub(crate) fn for_test(release: impl Future<Output = ()> + Send + 'static) -> Self {
        Self {
            inner: Arc::new(InferenceReservationInner {
                release: Mutex::new(Some(Box::pin(release))),
                reconcile: Mutex::new(None),
            }),
        }
    }

    pub(crate) fn reconcile(&self, actual_tokens: i64) {
        let reconcile = self
            .inner
            .reconcile
            .lock()
            .expect("HTTP reservation reconciliation mutex is not poisoned")
            .take();
        let Some(reconcile) = reconcile else {
            return;
        };
        gateway::limits::reconcile_tokens_in_background(
            reconcile.limiter,
            reconcile.lease,
            actual_tokens,
            Some((reconcile.store, reconcile.api_key_id)),
        );
    }

    pub(super) async fn release(self) {
        if let Some(release) = self.start_release() {
            let _ = release.await;
        }
    }

    pub(super) fn spawn_release(&self) {
        let _ = self.start_release();
    }

    fn start_release(&self) -> Option<tokio::task::JoinHandle<()>> {
        let release = self
            .inner
            .release
            .lock()
            .expect("HTTP reservation release mutex is not poisoned")
            .take()?;
        spawn_release_future(release)
    }
}

/// Spawns a reservation release future on the current Tokio runtime, returning
/// `None` when no runtime is available (e.g. the last owner drops off-runtime).
fn spawn_release_future(release: ReleaseFuture) -> Option<tokio::task::JoinHandle<()>> {
    let runtime = tokio::runtime::Handle::try_current().ok()?;
    Some(runtime.spawn(release))
}

impl Drop for InferenceReservationInner {
    fn drop(&mut self) {
        let Some(release) = self
            .release
            .get_mut()
            .expect("HTTP reservation release mutex is not poisoned")
            .take()
        else {
            return;
        };
        if spawn_release_future(release).is_none() {
            tracing::warn!("could not release HTTP concurrency lease outside a Tokio runtime");
        }
    }
}

pub(crate) struct ReleaseReservationBody {
    pub(crate) inner: Body,
    pub(crate) reservation: InferenceReservation,
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

#[derive(Clone)]
pub(crate) struct InferencePrincipal {
    runtime: Arc<RuntimeBundle>,
    lookup_id: ApiKeyLookupId,
    surface: Surface,
}

impl InferencePrincipal {
    pub(crate) fn runtime(&self) -> &Arc<RuntimeBundle> {
        &self.runtime
    }

    pub(crate) fn key(&self) -> &ApiKey {
        self.runtime
            .api_keys
            .get(&self.lookup_id)
            .expect("authenticated API key must remain in its pinned runtime")
    }

    pub(crate) fn lookup_id(&self) -> &ApiKeyLookupId {
        &self.lookup_id
    }

    pub(crate) const fn surface(&self) -> Surface {
        self.surface
    }

    #[cfg(test)]
    pub(crate) fn for_test(
        runtime: Arc<RuntimeBundle>,
        lookup_id: ApiKeyLookupId,
        surface: Surface,
    ) -> Self {
        Self {
            runtime,
            lookup_id,
            surface,
        }
    }
}

pub(super) fn authenticate_inference_headers(
    state: &GatewayState,
    headers: &HeaderMap,
    surface: Surface,
) -> Result<InferencePrincipal, crate::Problem> {
    let token = match surface {
        Surface::OpenAi => headers
            .get(axum::http::header::AUTHORIZATION)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.split_once(' '))
            .filter(|(scheme, token)| {
                scheme.eq_ignore_ascii_case("bearer")
                    && !token.is_empty()
                    && !token.contains(char::is_whitespace)
            })
            .map(|(_, token)| token),
        Surface::Anthropic => inference_header_token(headers, "x-api-key"),
        Surface::Gemini => inference_header_token(headers, "x-goog-api-key"),
    }
    .ok_or_else(|| crate::Problem::unauthorized("The API key is invalid or unavailable."))?;
    let auth_hmac_key = &state.auth_hmac_key;
    let lookup = auth_hmac_key
        .lookup_id(token)
        .map_err(|_| crate::Problem::unauthorized("The API key is invalid or unavailable."))?
        .to_owned();
    let lookup_id = ApiKeyLookupId::parse(&lookup)
        .map_err(|_| crate::Problem::unauthorized("The API key is invalid or unavailable."))?;
    let snapshot = state.runtime.pin();
    let key = snapshot
        .api_keys
        .get(&lookup_id)
        .ok_or_else(|| crate::Problem::unauthorized("The API key is invalid or unavailable."))?;
    auth_hmac_key
        .parse_and_verify(token, key.digest.as_bytes())
        .map_err(|_| crate::Problem::unauthorized("The API key is invalid or unavailable."))?;
    if key.status != ApiKeyStatus::Active
        || key
            .expires_at
            .is_some_and(|expires_at| expires_at <= snapshot.database_time())
    {
        return Err(crate::Problem::unauthorized(
            "The API key is invalid or unavailable.",
        ));
    }
    Ok(InferencePrincipal {
        lookup_id,
        runtime: snapshot,
        surface,
    })
}

pub(super) async fn reserve_http_inference_limits(
    state: &GatewayState,
    principal: &InferencePrincipal,
    requested_tokens: i64,
    requested_route: Option<&str>,
) -> Result<Option<InferenceReservation>, gateway::InferenceError> {
    if !principal.key().limits.has_hard_limits() {
        return Ok(None);
    }
    let tokens_per_minute = principal
        .key()
        .limits
        .tokens_per_minute
        .map(|value| i64::try_from(value.get()))
        .transpose()
        .map_err(|_| gateway::InferenceError::unavailable("limit_configuration_invalid"))?;
    let route_allowed = |slug: &RouteSlug| {
        principal.key().allowed_routes.is_empty() || principal.key().allowed_routes.contains(slug)
    };
    let route_timeout = requested_route
        .and_then(|slug| RouteSlug::parse(slug).ok())
        .as_ref()
        .filter(|slug| route_allowed(slug))
        .and_then(|slug| principal.runtime.routes.get(slug))
        .map(|route| route.overall_timeout.as_duration())
        .unwrap_or(Duration::from_millis(
            u64::try_from(MAX_ROUTE_TIMEOUT_MS).expect("route timeout maximum is positive"),
        ));
    reserve_http_limits(
        state,
        principal,
        tokens_per_minute,
        requested_tokens,
        route_timeout.saturating_add(Duration::from_secs(60)),
    )
    .await
}

pub(super) async fn reserve_http_multipart_limits(
    state: &GatewayState,
    principal: &InferencePrincipal,
) -> Result<Option<InferenceReservation>, gateway::InferenceError> {
    if principal.key().limits.requests_per_minute.is_none()
        && principal.key().limits.concurrency.is_none()
    {
        return Ok(None);
    }
    reserve_http_limits(
        state,
        principal,
        None,
        0,
        REQUEST_BODY_TIMEOUT.saturating_add(Duration::from_secs(60)),
    )
    .await
}

async fn reserve_http_limits(
    state: &GatewayState,
    principal: &InferencePrincipal,
    tokens_per_minute: Option<i64>,
    requested_tokens: i64,
    lease_ttl: Duration,
) -> Result<Option<InferenceReservation>, gateway::InferenceError> {
    let limiter = state
        .limiter
        .current()
        .ok_or_else(|| gateway::InferenceError::unavailable("distributed_limits_unavailable"))?;
    let result = tokio::time::timeout(
        Duration::from_secs(1),
        limiter.reserve(LimitRequest {
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
            requested_tokens,
            lease_ttl,
        }),
    )
    .await
    .map_err(|_| gateway::InferenceError::unavailable("distributed_limits_unavailable"))?;
    match result {
        Ok(lease) => Ok(Some(InferenceReservation::distributed(
            limiter,
            lease,
            state.store().clone(),
            principal.key().id.as_uuid(),
        ))),
        Err(LimitError::Exceeded {
            dimension,
            retry_after,
        }) => Err(gateway::InferenceError::rate_limited(
            dimension,
            retry_after,
        )),
        Err(error) => {
            tracing::error!(%error, "hard HTTP limit reservation failed closed");
            Err(gateway::InferenceError::unavailable(
                "distributed_limits_unavailable",
            ))
        }
    }
}

pub(crate) fn estimate_http_json_request_tokens(
    category: gateway::TokenEstimate,
    body: &[u8],
) -> i64 {
    let encoded_body = body.len().saturating_add(3) / 4;
    let baseline = if category == gateway::TokenEstimate::Generation {
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
    let embedding_token_floor = if category == gateway::TokenEstimate::Embeddings {
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

pub(super) const fn estimate_http_non_json_request_tokens(category: gateway::TokenEstimate) -> i64 {
    match category {
        gateway::TokenEstimate::Generation => 4_096,
        gateway::TokenEstimate::Transcription => 1_500,
        gateway::TokenEstimate::Media => 2_000,
        gateway::TokenEstimate::Default | gateway::TokenEstimate::Embeddings => 1,
    }
}

fn inference_header_token<'a>(headers: &'a HeaderMap, name: &'static str) -> Option<&'a str> {
    headers
        .get(name)
        .and_then(|value| value.to_str().ok())
        .filter(|token| !token.is_empty() && !token.contains(char::is_whitespace))
}
