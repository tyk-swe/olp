use chrono::Utc;
use olp_domain::{
    AttemptPlan, CanonicalEvent, CanonicalEventKind, CanonicalResult, OperationKind, RouteSlug,
    Surface,
};
use olp_storage::{
    request_metadata::RequestAttemptMetadata, request_metadata::RequestAttemptUsageMetadata,
    request_metadata::RequestMetadataEvent,
};
use rust_decimal::{Decimal, prelude::FromPrimitive as _};
use serde_json::Value;
use tracing::error;

use crate::{
    InferenceError, InferenceService,
    failover::AttemptLifecycleObserver,
    limits::{DistributedLimitReservation, InferenceReservation, release_limits},
    telemetry::{elapsed_ms, metadata_status_code},
};

/// Terminal accounting information supplied by a delivery adapter after it
/// has rendered a canonical result or event stream.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RequestOutcome {
    status_code: Option<u16>,
    error_class: Option<String>,
}

impl RequestOutcome {
    #[must_use]
    pub const fn success() -> Self {
        Self::success_with_status(200)
    }

    #[must_use]
    pub const fn success_with_status(status_code: u16) -> Self {
        Self {
            status_code: Some(status_code),
            error_class: None,
        }
    }

    #[must_use]
    pub fn failure(status_code: Option<u16>, error_class: impl Into<String>) -> Self {
        Self {
            status_code,
            error_class: Some(error_class.into()),
        }
    }

    #[must_use]
    pub(crate) fn from_error(error: &InferenceError) -> Self {
        Self::failure(
            (error.code() != "client_cancelled").then(|| metadata_status_code(error)),
            error.code(),
        )
    }

    #[must_use]
    pub(crate) fn provider_protocol_failure() -> Self {
        Self::failure(Some(502), "provider_protocol_error")
    }

    #[must_use]
    pub(crate) fn client_cancelled() -> Self {
        Self::failure(None, "client_cancelled")
    }
}

pub(crate) struct RequestAccountingInput {
    pub generation_id: uuid::Uuid,
    pub api_key_id: uuid::Uuid,
    pub request_id: uuid::Uuid,
    pub route_slug: RouteSlug,
    pub request_started_at: chrono::DateTime<Utc>,
    pub request_started: tokio::time::Instant,
    pub surface: Surface,
    pub operation: OperationKind,
}

struct ActiveRequestAttempt {
    ordinal: u16,
    provider_id: uuid::Uuid,
    upstream_model: String,
    started_at: chrono::DateTime<Utc>,
    started: tokio::time::Instant,
}

struct LimitCleanup {
    delta_lease: Option<DistributedLimitReservation>,
    admission_reservation: Option<InferenceReservation>,
    admission_reserved_tokens: Option<i64>,
    actual_tokens: Option<i64>,
}

impl LimitCleanup {
    async fn run(self) {
        let (admission_actual, delta_actual) =
            split_actual_tokens(self.actual_tokens, self.admission_reserved_tokens);
        if let (Some(reservation), Some(actual)) = (self.admission_reservation, admission_actual) {
            reservation.reconcile(actual).await;
        }
        release_limits(self.delta_lease, delta_actual).await;
    }
}

fn split_actual_tokens(
    actual_tokens: Option<i64>,
    admission_reserved_tokens: Option<i64>,
) -> (Option<i64>, Option<i64>) {
    match (actual_tokens, admission_reserved_tokens) {
        (Some(actual), Some(admission_reserved)) => (
            Some(actual.min(admission_reserved)),
            Some(actual.saturating_sub(admission_reserved).max(0)),
        ),
        (Some(actual), None) => (None, Some(actual)),
        (None, _) => (None, None),
    }
}

/// Cancellation-safe request accounting and limit cleanup owned by an active
/// inference execution.
pub struct RequestAccountingGuard {
    service: InferenceService,
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
    lease: Option<DistributedLimitReservation>,
    admission_reservation: Option<InferenceReservation>,
    admission_reserved_tokens: Option<i64>,
    active_attempt: Option<ActiveRequestAttempt>,
    armed: bool,
}

impl RequestAccountingGuard {
    pub(crate) fn new(
        service: InferenceService,
        input: RequestAccountingInput,
        lease: Option<DistributedLimitReservation>,
        admission_reservation: Option<InferenceReservation>,
        admission_reserved_tokens: Option<i64>,
    ) -> Self {
        Self {
            service,
            generation_id: input.generation_id,
            api_key_id: input.api_key_id,
            request_id: input.request_id,
            route_slug: input.route_slug,
            attempts: Vec::new(),
            request_started_at: input.request_started_at,
            request_started: input.request_started,
            attempt_started: None,
            first_byte_ms: None,
            committed: false,
            usage: UsageCapture::default(),
            surface: input.surface,
            operation: input.operation,
            lease,
            admission_reservation,
            admission_reserved_tokens,
            active_attempt: None,
            armed: true,
        }
    }

    pub(crate) fn record_attempt_started(
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

    pub(crate) fn record_attempts(
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

    fn record_attempt_completed(&mut self, attempt: &RequestAttemptMetadata) {
        self.attempts.push(attempt.clone());
        self.active_attempt = None;
    }

    #[must_use]
    pub fn usage_mut(&mut self) -> &mut UsageCapture {
        &mut self.usage
    }

    pub(crate) fn replace_usage(&mut self, usage: UsageCapture) {
        self.usage = usage;
    }

    pub async fn release_limits(&mut self) {
        let Some(cleanup) = self.take_limit_cleanup() else {
            return;
        };
        let task = tokio::spawn(cleanup.run());
        if let Err(error) = task.await {
            tracing::warn!(%error, "request limit cleanup task failed");
        }
    }

    pub async fn finish(mut self, outcome: RequestOutcome) {
        self.release_limits().await;
        self.emit(&outcome, true);
        self.armed = false;
    }

    pub(crate) fn into_finalizer(mut self) -> RequestMetadataFinalizer {
        self.armed = false;
        RequestMetadataFinalizer {
            service: self.service.clone(),
            generation_id: self.generation_id,
            api_key_id: self.api_key_id,
            request_id: self.request_id,
            route_slug: self.route_slug.clone(),
            attempts: self.attempts.clone(),
            request_started_at: self.request_started_at,
            request_started: self.request_started,
            attempt_started: self.attempt_started,
            first_byte_ms: self.first_byte_ms,
            committed: self.committed,
            usage: self.usage.clone(),
            surface: self.surface,
            operation: self.operation,
        }
    }

    fn take_limit_cleanup(&mut self) -> Option<LimitCleanup> {
        let delta_lease = self.lease.take();
        let admission_reservation = self.admission_reservation.take();
        if delta_lease.is_none() && admission_reservation.is_none() {
            return None;
        }
        Some(LimitCleanup {
            delta_lease,
            admission_reservation,
            admission_reserved_tokens: self.admission_reserved_tokens,
            actual_tokens: self.usage.actual_tokens(),
        })
    }

    fn emit(&self, outcome: &RequestOutcome, finalize_attempt: bool) {
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
                usage: Some(RequestAttemptUsageMetadata {
                    observed: false,
                    complete: false,
                    billing_uncertain: true,
                    input_tokens: None,
                    output_tokens: None,
                    cached_input_tokens: None,
                    media_units: None,
                }),
            });
        }
        emit_request_metadata_event(
            &self.service,
            RequestMetadataInput {
                generation_id: self.generation_id,
                api_key_id: self.api_key_id,
                request_id: self.request_id,
                route_slug: &self.route_slug,
                attempts: &attempts,
                request_started_at: self.request_started_at,
                request_started: self.request_started,
                final_attempt_started: finalize_attempt.then_some(self.attempt_started).flatten(),
                first_byte_ms: self.first_byte_ms,
                status_code: outcome.status_code,
                error_class: outcome.error_class.clone(),
                committed: self.committed,
                usage: &self.usage,
                surface: self.surface,
                operation: self.operation,
            },
        );
    }
}

impl AttemptLifecycleObserver for RequestAccountingGuard {
    fn on_attempt_started(
        &mut self,
        completed: &[RequestAttemptMetadata],
        attempt: &AttemptPlan,
        ordinal: u16,
        started_at: chrono::DateTime<Utc>,
        started: tokio::time::Instant,
    ) {
        self.record_attempt_started(
            completed,
            ordinal,
            attempt.provider_id.as_uuid(),
            &attempt.upstream_model,
            started_at,
            started,
        );
    }

    fn on_attempt_completed(&mut self, attempt: &RequestAttemptMetadata) {
        self.record_attempt_completed(attempt);
    }
}

impl Drop for RequestAccountingGuard {
    fn drop(&mut self) {
        if !self.armed {
            return;
        }
        self.emit(&RequestOutcome::client_cancelled(), false);
        let Some(cleanup) = self.take_limit_cleanup() else {
            return;
        };
        if let Ok(runtime) = tokio::runtime::Handle::try_current() {
            runtime.spawn(cleanup.run());
        } else {
            tracing::warn!("request limit cleanup skipped outside a Tokio runtime");
        }
    }
}

pub(crate) struct RequestMetadataFinalizer {
    service: InferenceService,
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
}

impl RequestMetadataFinalizer {
    pub(crate) fn finalize(self, outcome: &RequestOutcome) {
        emit_request_metadata_event(
            &self.service,
            RequestMetadataInput {
                generation_id: self.generation_id,
                api_key_id: self.api_key_id,
                request_id: self.request_id,
                route_slug: &self.route_slug,
                attempts: &self.attempts,
                request_started_at: self.request_started_at,
                request_started: self.request_started,
                final_attempt_started: self.attempt_started,
                first_byte_ms: self.first_byte_ms,
                status_code: outcome.status_code,
                error_class: outcome.error_class.clone(),
                committed: self.committed,
                usage: &self.usage,
                surface: self.surface,
                operation: self.operation,
            },
        );
    }
}

#[derive(Clone, Default)]
pub struct UsageCapture {
    observed: bool,
    complete: bool,
    input_tokens: Option<i64>,
    output_tokens: Option<i64>,
    cached_input_tokens: Option<i64>,
    media_units: Option<Decimal>,
}

impl UsageCapture {
    #[must_use]
    pub fn actual_tokens(&self) -> Option<i64> {
        self.input_tokens?.checked_add(self.output_tokens?)
    }

    pub fn observe(&mut self, event: &CanonicalEvent) {
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

    pub fn observe_openai_media_event(&mut self, event: &CanonicalEvent) {
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

pub(crate) fn usage_from_result(result: &CanonicalResult) -> UsageCapture {
    let (usage, media_units) = match result {
        CanonicalResult::Embeddings(result) => (result.usage, None),
        CanonicalResult::Images(result) => (result.usage, Decimal::from_usize(result.images.len())),
        CanonicalResult::Transcription(result) => (
            None,
            result.duration_seconds.and_then(Decimal::from_f64_retain),
        ),
        CanonicalResult::VideoJob(result) => (
            None,
            result
                .seconds
                .as_deref()
                .and_then(|value| value.parse::<Decimal>().ok()),
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

struct RequestMetadataInput<'a> {
    generation_id: uuid::Uuid,
    api_key_id: uuid::Uuid,
    request_id: uuid::Uuid,
    route_slug: &'a RouteSlug,
    attempts: &'a [RequestAttemptMetadata],
    request_started_at: chrono::DateTime<Utc>,
    request_started: tokio::time::Instant,
    final_attempt_started: Option<tokio::time::Instant>,
    first_byte_ms: Option<u64>,
    status_code: Option<u16>,
    error_class: Option<String>,
    committed: bool,
    usage: &'a UsageCapture,
    surface: Surface,
    operation: OperationKind,
}

struct FinalAttemptUpdate<'a> {
    completed_at: chrono::DateTime<Utc>,
    started: tokio::time::Instant,
    first_byte_ms: Option<u64>,
    status_code: Option<u16>,
    error_class: &'a Option<String>,
    committed: bool,
    usage: &'a UsageCapture,
}

fn update_final_attempt(attempt: &mut RequestAttemptMetadata, update: FinalAttemptUpdate<'_>) {
    attempt.completed_at = update.completed_at.max(attempt.started_at);
    attempt.status_code = update.status_code;
    attempt.error_class.clone_from(update.error_class);
    attempt.committed = update.committed;
    attempt.latency_ms = elapsed_ms(update.started.elapsed());
    attempt.first_byte_ms = update.first_byte_ms;
    if update.usage.observed {
        attempt.usage = Some(RequestAttemptUsageMetadata {
            observed: true,
            complete: update.usage.complete,
            billing_uncertain: false,
            input_tokens: update.usage.input_tokens,
            output_tokens: update.usage.output_tokens,
            cached_input_tokens: update.usage.cached_input_tokens,
            media_units: update.usage.media_units,
        });
    }
}

fn emit_request_metadata_event(service: &InferenceService, input: RequestMetadataInput<'_>) {
    let Some(emitter) = service.request_metadata() else {
        return;
    };
    let request_completed_at = Utc::now();
    let mut attempts = input.attempts.to_vec();
    if let (Some(final_attempt), Some(started)) = (attempts.last_mut(), input.final_attempt_started)
    {
        update_final_attempt(
            final_attempt,
            FinalAttemptUpdate {
                completed_at: request_completed_at,
                started,
                first_byte_ms: input.first_byte_ms,
                status_code: input.status_code,
                error_class: &input.error_class,
                committed: input.committed,
                usage: input.usage,
            },
        );
    }
    let provider_id = attempts.last().map(|attempt| attempt.provider_id);
    let upstream_model = attempts
        .last()
        .map(|attempt| attempt.upstream_model.clone());
    let result = emitter.emit(RequestMetadataEvent {
        event_id: uuid::Uuid::now_v7(),
        request_id: input.request_id,
        runtime_generation_id: input.generation_id,
        api_key_id: input.api_key_id,
        provider_id,
        route_slug: input.route_slug.to_string(),
        upstream_model,
        operation: input.operation,
        surface: input.surface,
        request_started_at: input.request_started_at,
        request_completed_at,
        observed_at: request_completed_at,
        status_code: input.status_code,
        error_class: input.error_class,
        committed: input.committed,
        latency_ms: elapsed_ms(input.request_started.elapsed()),
        first_byte_ms: input.first_byte_ms,
        input_tokens: input.usage.input_tokens,
        output_tokens: input.usage.output_tokens,
        cached_input_tokens: input.usage.cached_input_tokens,
        media_units: input.usage.media_units,
        usage_complete: input.usage.observed && input.usage.complete,
        unpriced: true,
        attempts,
    });
    if result.is_err() {
        error!(request_id = %input.request_id, "request metadata buffer overflowed");
    }
}

#[cfg(test)]
mod tests {
    use super::{
        FinalAttemptUpdate, RequestAttemptMetadata, RequestAttemptUsageMetadata, RequestOutcome,
        UsageCapture, split_actual_tokens, update_final_attempt,
    };
    use chrono::Utc;
    use uuid::Uuid;

    #[test]
    fn successful_outcome_preserves_the_http_status() {
        let outcome = RequestOutcome::success_with_status(201);
        assert_eq!(outcome.status_code, Some(201));
        assert_eq!(outcome.error_class, None);
    }

    #[test]
    fn actual_tokens_are_split_across_admission_and_delta_reservations() {
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
    fn final_streaming_usage_is_attached_to_the_final_attempt() {
        let mut attempt = uncertain_attempt();
        let usage = UsageCapture {
            observed: true,
            complete: true,
            input_tokens: Some(12),
            output_tokens: Some(7),
            cached_input_tokens: Some(2),
            media_units: None,
        };
        let no_error = None;
        update_final_attempt(
            &mut attempt,
            FinalAttemptUpdate {
                completed_at: Utc::now(),
                started: tokio::time::Instant::now(),
                first_byte_ms: Some(3),
                status_code: Some(200),
                error_class: &no_error,
                committed: true,
                usage: &usage,
            },
        );
        let attempt_usage = attempt.usage.unwrap();
        assert!(attempt_usage.observed);
        assert!(attempt_usage.complete);
        assert!(!attempt_usage.billing_uncertain);
        assert_eq!(attempt_usage.input_tokens, Some(12));
        assert_eq!(attempt_usage.output_tokens, Some(7));
    }

    #[test]
    fn cancellation_after_commitment_preserves_billing_uncertainty() {
        let mut attempt = uncertain_attempt();
        let error = Some("client_cancelled".to_owned());
        update_final_attempt(
            &mut attempt,
            FinalAttemptUpdate {
                completed_at: Utc::now(),
                started: tokio::time::Instant::now(),
                first_byte_ms: Some(3),
                status_code: None,
                error_class: &error,
                committed: true,
                usage: &UsageCapture::default(),
            },
        );
        let attempt_usage = attempt.usage.unwrap();
        assert!(!attempt_usage.observed);
        assert!(!attempt_usage.complete);
        assert!(attempt_usage.billing_uncertain);
        assert!(attempt.committed);
    }

    fn uncertain_attempt() -> RequestAttemptMetadata {
        let now = Utc::now();
        RequestAttemptMetadata {
            id: Uuid::now_v7(),
            ordinal: 1,
            provider_id: Uuid::now_v7(),
            upstream_model: "mock-model".to_owned(),
            started_at: now,
            completed_at: now,
            status_code: Some(200),
            error_class: None,
            committed: true,
            latency_ms: 0,
            first_byte_ms: None,
            usage: Some(RequestAttemptUsageMetadata {
                observed: false,
                complete: false,
                billing_uncertain: true,
                input_tokens: None,
                output_tokens: None,
                cached_input_tokens: None,
                media_units: None,
            }),
        }
    }
}
