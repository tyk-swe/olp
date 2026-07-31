use std::time::Duration;

use axum::http::StatusCode;
use chrono::Utc;
use olp_domain::{
    CanonicalEvent, CanonicalEventKind, CanonicalResult, OperationKind, RouteSlug, Surface,
};
use olp_storage::{LimitLease, RequestAttemptMetadata, RequestMetadataEvent};
use rust_decimal::{Decimal, prelude::FromPrimitive as _};
use serde_json::Value;
use tracing::error;

use crate::{GatewayState, request_admission::InferenceReservation};

use super::{error::InferenceError, execution::RoutedEventExecution, limits::release_limits};

pub(super) fn outcome_status_code(failure: Option<&InferenceError>) -> Option<u16> {
    failure.map_or(Some(StatusCode::OK.as_u16()), |error| {
        (error.code != "client_cancelled").then_some(error.status.as_u16())
    })
}

pub(crate) fn emit_event_execution_metadata(
    state: &GatewayState,
    execution: &RoutedEventExecution,
    usage: &UsageCapture,
    failure: Option<&InferenceError>,
) {
    emit_request_metadata_event(
        state,
        execution.generation_id,
        execution.api_key_id,
        execution.request_id,
        &execution.route_slug,
        &execution.attempts,
        execution.request_started_at,
        execution.request_started,
        Some(execution.attempt_started),
        Some(execution.first_byte_ms),
        outcome_status_code(failure),
        failure.map(|error| error.code.to_owned()),
        true,
        usage,
        execution.surface,
        execution.operation_kind,
    );
}

pub(super) struct UnaryRequestMetadataFinalizer {
    pub(super) state: GatewayState,
    pub(super) generation_id: uuid::Uuid,
    pub(super) api_key_id: uuid::Uuid,
    pub(super) request_id: uuid::Uuid,
    pub(super) route_slug: RouteSlug,
    pub(super) attempts: Vec<RequestAttemptMetadata>,
    pub(super) request_started_at: chrono::DateTime<Utc>,
    pub(super) request_started: tokio::time::Instant,
    pub(super) attempt_started: tokio::time::Instant,
    pub(super) first_byte_ms: u64,
    pub(super) usage: UsageCapture,
    pub(super) surface: Surface,
    pub(super) operation: OperationKind,
}

impl UnaryRequestMetadataFinalizer {
    pub(super) fn finalize(self, failure: Option<&InferenceError>) {
        emit_request_metadata_event(
            &self.state,
            self.generation_id,
            self.api_key_id,
            self.request_id,
            &self.route_slug,
            &self.attempts,
            self.request_started_at,
            self.request_started,
            Some(self.attempt_started),
            Some(self.first_byte_ms),
            outcome_status_code(failure),
            failure.map(|error| error.code.to_owned()),
            true,
            &self.usage,
            self.surface,
            self.operation,
        );
    }
}

pub(crate) struct RequestAccountingGuard {
    state: GatewayState,
    generation_id: uuid::Uuid,
    api_key_id: uuid::Uuid,
    request_id: uuid::Uuid,
    route_slug: RouteSlug,
    attempts: Vec<RequestAttemptMetadata>,
    request_started_at: chrono::DateTime<Utc>,
    request_started: tokio::time::Instant,
    attempt_started: Option<tokio::time::Instant>,
    first_byte_ms: Option<u64>,
    committed: bool,
    usage: UsageCapture,
    surface: Surface,
    operation: OperationKind,
    lease: Option<LimitLease>,
    http_reservation: Option<InferenceReservation>,
    http_reserved_tokens: Option<i64>,
    active_attempt: Option<ActiveRequestAttempt>,
    armed: bool,
}

struct ActiveRequestAttempt {
    ordinal: u16,
    provider_id: uuid::Uuid,
    upstream_model: String,
    started_at: chrono::DateTime<Utc>,
    started: tokio::time::Instant,
}

struct LimitCleanup {
    state: GatewayState,
    delta_lease: Option<LimitLease>,
    http_reservation: Option<InferenceReservation>,
    http_reserved_tokens: Option<i64>,
    actual_tokens: Option<i64>,
}

impl LimitCleanup {
    async fn run(self) {
        let (http_actual, delta_actual) =
            split_actual_tokens(self.actual_tokens, self.http_reserved_tokens);
        if let (Some(reservation), Some(actual)) = (self.http_reservation, http_actual) {
            reservation.reconcile(actual).await;
        }
        release_limits(&self.state, self.delta_lease.as_ref(), delta_actual).await;
    }
}

fn split_actual_tokens(
    actual_tokens: Option<i64>,
    http_reserved_tokens: Option<i64>,
) -> (Option<i64>, Option<i64>) {
    match (actual_tokens, http_reserved_tokens) {
        (Some(actual), Some(http_reserved)) => (
            Some(actual.min(http_reserved)),
            Some(actual.saturating_sub(http_reserved).max(0)),
        ),
        (Some(actual), None) => (None, Some(actual)),
        (None, _) => (None, None),
    }
}

impl RequestAccountingGuard {
    #[allow(clippy::too_many_arguments)]
    pub(super) fn new(
        state: &GatewayState,
        generation_id: uuid::Uuid,
        api_key_id: uuid::Uuid,
        request_id: uuid::Uuid,
        route_slug: RouteSlug,
        request_started_at: chrono::DateTime<Utc>,
        request_started: tokio::time::Instant,
        surface: Surface,
        operation: OperationKind,
        lease: Option<LimitLease>,
    ) -> Self {
        crate::claim_http_inference_metadata();
        Self {
            state: state.clone(),
            generation_id,
            api_key_id,
            request_id,
            route_slug,
            attempts: Vec::new(),
            request_started_at,
            request_started,
            attempt_started: None,
            first_byte_ms: None,
            committed: false,
            usage: UsageCapture::default(),
            surface,
            operation,
            lease,
            http_reservation: crate::http_inference_reservation(),
            http_reserved_tokens: crate::http_inference_reserved_tokens(),
            active_attempt: None,
            armed: true,
        }
    }

    pub(super) fn record_attempt_started(
        &mut self,
        completed: &[RequestAttemptMetadata],
        ordinal: u16,
        provider_id: uuid::Uuid,
        upstream_model: &str,
        started_at: chrono::DateTime<Utc>,
        started: tokio::time::Instant,
    ) {
        self.attempts = completed.to_vec();
        self.active_attempt = Some(ActiveRequestAttempt {
            ordinal,
            provider_id,
            upstream_model: upstream_model.to_owned(),
            started_at,
            started,
        });
    }

    pub(super) fn record_attempts(
        &mut self,
        attempts: Vec<RequestAttemptMetadata>,
        attempt_started: Option<tokio::time::Instant>,
        first_byte_ms: Option<u64>,
        committed: bool,
    ) {
        self.attempts = attempts;
        self.active_attempt = None;
        self.attempt_started = attempt_started;
        self.first_byte_ms = first_byte_ms;
        self.committed = committed;
    }

    pub(crate) fn usage_mut(&mut self) -> &mut UsageCapture {
        &mut self.usage
    }

    pub(super) fn replace_usage(&mut self, usage: UsageCapture) {
        self.usage = usage;
    }

    pub(crate) async fn release_lease(&mut self) {
        let Some(cleanup) = self.take_limit_cleanup() else {
            return;
        };
        let task = tokio::spawn(cleanup.run());
        if let Err(error) = task.await {
            tracing::warn!(%error, "request limit cleanup task failed");
        }
    }

    pub(crate) async fn finish(mut self, failure: Option<&InferenceError>) {
        self.release_lease().await;
        self.emit(failure, true);
        self.armed = false;
    }

    pub(crate) fn disarm(mut self) -> Option<LimitLease> {
        self.armed = false;
        self.lease.take()
    }

    fn take_limit_cleanup(&mut self) -> Option<LimitCleanup> {
        let delta_lease = self.lease.take();
        let http_reservation = self.http_reservation.take();
        if delta_lease.is_none() && http_reservation.is_none() {
            return None;
        }
        Some(LimitCleanup {
            state: self.state.clone(),
            delta_lease,
            http_reservation,
            http_reserved_tokens: self.http_reserved_tokens,
            actual_tokens: self.usage.actual_tokens(),
        })
    }

    fn emit(&self, failure: Option<&InferenceError>, finalize_attempt: bool) {
        let mut attempts = self.attempts.clone();
        if let Some(active) = &self.active_attempt {
            attempts.push(RequestAttemptMetadata {
                id: uuid::Uuid::now_v7(),
                ordinal: active.ordinal,
                provider_id: active.provider_id,
                upstream_model: active.upstream_model.clone(),
                started_at: active.started_at,
                completed_at: Utc::now(),
                status_code: None,
                error_class: Some("cancelled".to_owned()),
                committed: false,
                latency_ms: elapsed_ms(active.started.elapsed()),
                first_byte_ms: None,
            });
        }
        emit_request_metadata_event(
            &self.state,
            self.generation_id,
            self.api_key_id,
            self.request_id,
            &self.route_slug,
            &attempts,
            self.request_started_at,
            self.request_started,
            finalize_attempt.then_some(self.attempt_started).flatten(),
            self.first_byte_ms,
            outcome_status_code(failure),
            failure.map(|error| error.code.to_owned()),
            self.committed,
            &self.usage,
            self.surface,
            self.operation,
        );
    }
}

impl Drop for RequestAccountingGuard {
    fn drop(&mut self) {
        if !self.armed {
            return;
        }
        let failure = InferenceError::client_cancelled();
        // A completed provider attempt already has its own terminal outcome.
        // Client cancellation after that point must not overwrite it.
        self.emit(Some(&failure), false);
        let Some(cleanup) = self.take_limit_cleanup() else {
            return;
        };
        // Mirrors request_admission::spawn_release_future: a guard dropped
        // outside a Tokio runtime (e.g. during runtime teardown) must not
        // panic in Drop; the lease TTL then reclaims the reservation.
        if let Ok(runtime) = tokio::runtime::Handle::try_current() {
            runtime.spawn(cleanup.run());
        } else {
            tracing::warn!("request limit cleanup skipped outside a Tokio runtime");
        }
    }
}

pub(super) fn usage_from_result(result: &CanonicalResult) -> UsageCapture {
    let (usage, media_units) = match result {
        CanonicalResult::Embeddings(result) => (result.usage, None),
        CanonicalResult::Images(result) => (result.usage, Decimal::from_usize(result.images.len())),
        CanonicalResult::Transcription(result) => (
            None,
            result
                .duration_seconds
                .and_then(Decimal::from_f64_retain)
                .and_then(valid_media_units),
        ),
        CanonicalResult::VideoJob(result) => (
            None,
            result
                .seconds
                .as_deref()
                .and_then(|value| value.parse::<Decimal>().ok())
                .and_then(valid_media_units),
        ),
        CanonicalResult::TokenCount(result) => (
            Some(olp_domain::Usage {
                input_tokens: result.input_tokens,
                output_tokens: 0,
                total_tokens: result.input_tokens,
                cached_input_tokens: None,
                reasoning_tokens: None,
            }),
            None,
        ),
        _ => (None, None),
    };
    if usage.is_none() && media_units.is_none() {
        return UsageCapture::default();
    }
    let (input_tokens, output_tokens, cached_input_tokens, token_complete) =
        usage.map_or((None, None, None, true), |usage| {
            let input = i64::try_from(usage.input_tokens).ok();
            let output = i64::try_from(usage.output_tokens).ok();
            let cached = usage
                .cached_input_tokens
                .and_then(|value| i64::try_from(value).ok());
            let complete = input.is_some()
                && output.is_some()
                && (usage.cached_input_tokens.is_none() || cached.is_some());
            (input, output, cached, complete)
        });
    UsageCapture {
        observed: true,
        complete: token_complete,
        input_tokens,
        output_tokens,
        cached_input_tokens,
        media_units,
    }
}

fn valid_media_units(value: Decimal) -> Option<Decimal> {
    (!value.is_sign_negative()).then_some(value)
}

#[derive(Clone, Default)]
pub(crate) struct UsageCapture {
    observed: bool,
    complete: bool,
    input_tokens: Option<i64>,
    output_tokens: Option<i64>,
    cached_input_tokens: Option<i64>,
    media_units: Option<Decimal>,
}

impl UsageCapture {
    pub(crate) fn actual_tokens(&self) -> Option<i64> {
        self.input_tokens?.checked_add(self.output_tokens?)
    }

    pub(crate) fn observe(&mut self, event: &CanonicalEvent) {
        let CanonicalEventKind::Usage { usage } = &event.kind else {
            return;
        };
        self.observed = true;
        self.input_tokens = i64::try_from(usage.input_tokens).ok();
        self.output_tokens = i64::try_from(usage.output_tokens).ok();
        self.cached_input_tokens = usage
            .cached_input_tokens
            .and_then(|value| i64::try_from(value).ok());
        self.complete = self.input_tokens.is_some()
            && self.output_tokens.is_some()
            && (usage.cached_input_tokens.is_none() || self.cached_input_tokens.is_some());
    }

    pub(super) fn observe_openai_media_event(&mut self, event: &CanonicalEvent) {
        let CanonicalEventKind::SourceExtension { extensions } = &event.kind else {
            return;
        };
        if extensions.source != Some(Surface::OpenAi) {
            return;
        }
        let Some(usage) = extensions
            .values
            .get("/__olp/raw_sse/data")
            .and_then(|value| value.get("usage"))
        else {
            return;
        };
        let input = usage
            .get("input_tokens")
            .or_else(|| usage.get("prompt_tokens"))
            .and_then(Value::as_u64)
            .and_then(|value| i64::try_from(value).ok());
        let output = usage
            .get("output_tokens")
            .or_else(|| usage.get("completion_tokens"))
            .and_then(Value::as_u64)
            .and_then(|value| i64::try_from(value).ok());
        let cached = usage
            .get("input_tokens_details")
            .or_else(|| usage.get("prompt_tokens_details"))
            .and_then(|details| details.get("cached_tokens"))
            .and_then(Value::as_u64)
            .and_then(|value| i64::try_from(value).ok());
        if input.is_none() && output.is_none() && cached.is_none() {
            return;
        }
        self.observed = true;
        self.input_tokens = input;
        self.output_tokens = output;
        self.cached_input_tokens = cached;
        self.complete = input.is_some() && output.is_some();
    }
}

#[allow(clippy::too_many_arguments)]
pub(super) fn emit_request_metadata_event(
    state: &GatewayState,
    generation_id: uuid::Uuid,
    api_key_id: uuid::Uuid,
    request_id: uuid::Uuid,
    route_slug: &RouteSlug,
    attempts: &[RequestAttemptMetadata],
    request_started_at: chrono::DateTime<Utc>,
    request_started: tokio::time::Instant,
    final_attempt_started: Option<tokio::time::Instant>,
    first_byte_ms: Option<u64>,
    status_code: Option<u16>,
    error_class: Option<String>,
    committed: bool,
    usage: &UsageCapture,
    surface: Surface,
    operation: OperationKind,
) {
    crate::claim_http_inference_metadata();
    if let Some(emitter) = &state.request_metadata {
        let request_completed_at = Utc::now();
        let mut attempts = attempts.to_vec();
        if let (Some(final_attempt), Some(started)) = (attempts.last_mut(), final_attempt_started) {
            final_attempt.completed_at = request_completed_at.max(final_attempt.started_at);
            final_attempt.status_code = status_code;
            final_attempt.error_class.clone_from(&error_class);
            final_attempt.committed = committed;
            final_attempt.latency_ms = elapsed_ms(started.elapsed());
            final_attempt.first_byte_ms = first_byte_ms;
        }
        let provider_id = attempts.last().map(|attempt| attempt.provider_id);
        let upstream_model = attempts
            .last()
            .map(|attempt| attempt.upstream_model.clone());
        let result = emitter.emit(RequestMetadataEvent {
            event_id: uuid::Uuid::now_v7(),
            request_id,
            runtime_generation_id: generation_id,
            api_key_id,
            provider_id,
            route_slug: route_slug.to_string(),
            upstream_model,
            operation,
            surface,
            request_started_at,
            request_completed_at,
            observed_at: request_completed_at,
            status_code,
            error_class,
            committed,
            latency_ms: elapsed_ms(request_started.elapsed()),
            first_byte_ms,
            input_tokens: usage.input_tokens,
            output_tokens: usage.output_tokens,
            cached_input_tokens: usage.cached_input_tokens,
            media_units: usage.media_units,
            usage_complete: usage.observed && usage.complete,
            attempts,
        });
        if result.is_err() {
            error!(%request_id, "request metadata buffer overflowed");
        }
    }
}

pub(super) fn elapsed_ms(duration: Duration) -> u64 {
    u64::try_from(duration.as_millis()).unwrap_or(u64::MAX)
}

#[cfg(test)]
mod tests {
    use rust_decimal::Decimal;

    use super::{split_actual_tokens, valid_media_units};

    #[test]
    fn actual_tokens_are_split_across_http_and_delta_reservations() {
        assert_eq!(
            split_actual_tokens(Some(40), Some(100)),
            (Some(40), Some(0))
        );
        assert_eq!(
            split_actual_tokens(Some(130), Some(100)),
            (Some(100), Some(30))
        );
        assert_eq!(split_actual_tokens(Some(40), None), (None, Some(40)));
        assert_eq!(split_actual_tokens(None, Some(100)), (None, None));
    }

    #[test]
    fn media_usage_must_be_nonnegative() {
        for (value, expected) in [
            (Decimal::new(-1, 0), None),
            (Decimal::ZERO, Some(Decimal::ZERO)),
            (Decimal::ONE, Some(Decimal::ONE)),
        ] {
            assert_eq!(valid_media_units(value), expected);
        }
    }
}
