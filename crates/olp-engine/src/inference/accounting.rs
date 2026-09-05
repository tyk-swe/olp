use crate::domain::{
    canonical::{
        events::{Event, Kind},
        identity::{OperationKind, Surface},
        results::CanonicalResult,
    },
    ids::RouteSlug,
};
use chrono::Utc;
use rust_decimal::{Decimal, prelude::FromPrimitive as _};
use serde_json::Value;
use tracing::error;

use crate::inference::{
    error::Error as InferenceError,
    limits::{Reservation, release},
    request_metadata::{
        Event as MetadataEvent, RequestAttemptMetadata, RequestAttemptUsageMetadata,
    },
    service::Service,
    telemetry::{elapsed_ms, metadata_status_code},
    tracing::{AttemptTrace, RequestTrace},
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
    pub(in crate::inference) fn from_error(error: &InferenceError) -> Self {
        Self::failure(
            (error.code() != "client_cancelled").then(|| metadata_status_code(error)),
            error.code(),
        )
    }

    #[must_use]
    pub(in crate::inference) fn provider_protocol_failure() -> Self {
        Self::failure(Some(502), "provider_protocol_error")
    }

    #[must_use]
    pub(in crate::inference) fn client_cancelled() -> Self {
        Self::failure(None, "client_cancelled")
    }
}

pub(in crate::inference) struct RequestAccountingInput {
    pub generation_id: uuid::Uuid,
    pub api_key_id: uuid::Uuid,
    pub request_id: uuid::Uuid,
    pub route_slug: RouteSlug,
    pub request_started_at: chrono::DateTime<Utc>,
    pub request_started: tokio::time::Instant,
    pub surface: Surface,
    pub operation: OperationKind,
    pub trace: Option<RequestTrace>,
}

struct ActiveRequestAttempt {
    ordinal: u16,
    provider_id: uuid::Uuid,
    upstream_model: String,
    started_at: chrono::DateTime<Utc>,
    started: tokio::time::Instant,
}

struct LimitCleanup {
    delta_lease: Option<Reservation>,
    admission_reservation: Option<Reservation>,
    admission_reserved_tokens: Option<i64>,
    actual_tokens: Option<i64>,
}

impl LimitCleanup {
    async fn run(self) {
        let (admission_actual, delta_actual) = split_actual_tokens(
            self.actual_tokens,
            self.admission_reserved_tokens,
            self.admission_reservation.is_some(),
            self.delta_lease.is_some(),
        );
        let delta_release = self.delta_lease.clone();
        tokio::join!(
            async {
                if let (Some(reservation), Some(actual)) =
                    (self.admission_reservation, admission_actual)
                {
                    reservation.reconcile(actual).await;
                }
                if let (Some(reservation), Some(actual)) = (self.delta_lease, delta_actual) {
                    reservation.reconcile(actual).await;
                }
            },
            release(delta_release, None),
        );
    }
}

fn split_actual_tokens(
    actual_tokens: Option<i64>,
    admission_reserved_tokens: Option<i64>,
    has_admission_reservation: bool,
    has_delta_lease: bool,
) -> (Option<i64>, Option<i64>) {
    match (
        actual_tokens,
        has_admission_reservation,
        has_delta_lease,
        admission_reserved_tokens,
    ) {
        (Some(actual), true, true, Some(admission_reserved)) => (
            Some(actual.min(admission_reserved)),
            Some(actual.saturating_sub(admission_reserved).max(0)),
        ),
        (Some(actual), true, true, None) => (Some(actual), Some(0)),
        (Some(actual), true, false, _) => (Some(actual), None),
        (Some(actual), false, true, _) => (None, Some(actual)),
        (Some(_), false, false, _) | (None, _, _, _) => (None, None),
    }
}

/// Cancellation-safe request accounting and limit cleanup owned by an active
/// inference execution.
pub struct RequestAccountingGuard {
    service: Service,
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
    lease: Option<Reservation>,
    admission_reservation: Option<Reservation>,
    admission_reserved_tokens: Option<i64>,
    active_attempt: Option<ActiveRequestAttempt>,
    request_trace: Option<RequestTrace>,
    attempt_trace: Option<AttemptTrace>,
    armed: bool,
}

impl RequestAccountingGuard {
    pub(in crate::inference) fn new(
        service: Service,
        input: RequestAccountingInput,
        lease: Option<Reservation>,
        admission_reservation: Option<Reservation>,
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
            request_trace: input.trace,
            attempt_trace: None,
            armed: true,
        }
    }

    pub(in crate::inference) fn record_attempt_started(
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

    pub(in crate::inference) fn record_attempts(
        &mut self,
        attempts: Vec<RequestAttemptMetadata>,
        attempt_started: Option<tokio::time::Instant>,
        first_byte_ms: Option<u64>,
        committed: bool,
        attempt_trace: Option<AttemptTrace>,
    ) {
        self.attempts = attempts;
        self.active_attempt = None;
        self.attempt_started = attempt_started;
        self.first_byte_ms = first_byte_ms;
        self.committed = committed;
        self.attempt_trace = attempt_trace;
    }

    #[must_use]
    pub fn usage_mut(&mut self) -> &mut UsageCapture {
        &mut self.usage
    }

    pub(in crate::inference) fn replace_usage(&mut self, usage: UsageCapture) {
        self.usage = usage;
    }

    pub(in crate::inference) fn finish_provider_attempt(&mut self, outcome: &RequestOutcome) {
        let Some(mut trace) = self.attempt_trace.take() else {
            return;
        };
        self.usage.record_trace(&trace);
        let outcome_class = match outcome.error_class.as_deref() {
            Some("client_cancelled") => "cancelled",
            Some(error_class) => error_class,
            None => "success",
        };
        trace.finish(outcome_class, Some("2xx"));
    }

    /// Hands the limit reservations to a cleanup task without waiting for
    /// it. Reconciling and releasing retry with backoff when Valkey is
    /// unreachable, and that latency must never sit on the response path.
    pub fn release(&mut self) {
        self.spawn_limit_cleanup(false);
    }

    /// Closes both spans for a terminal outcome. Every exit — success,
    /// finalizer, and the cancellation `Drop` — goes through here so a new
    /// terminal attribute cannot be added to only some of them.
    fn record_terminal_traces(&mut self, outcome: &RequestOutcome) {
        self.finish_provider_attempt(outcome);
        self.record_request_trace(outcome);
    }

    pub fn finish(mut self, outcome: RequestOutcome) {
        self.record_terminal_traces(&outcome);
        self.spawn_limit_cleanup(outcome.error_class.is_some());
        self.emit(&outcome, true);
        self.armed = false;
    }

    pub(in crate::inference) fn into_finalizer(mut self) -> RequestMetadataFinalizer {
        self.armed = false;
        RequestMetadataFinalizer(self)
    }

    fn take_limit_cleanup(&mut self, failed: bool) -> Option<LimitCleanup> {
        let delta_lease = self.lease.take();
        let admission_reservation = self.admission_reservation.take();
        if delta_lease.is_none() && admission_reservation.is_none() {
            return None;
        }
        Some(LimitCleanup {
            delta_lease,
            admission_reservation,
            admission_reserved_tokens: self.admission_reserved_tokens,
            actual_tokens: self.usage.actual_tokens().or_else(|| {
                (failed && self.active_attempt.is_none() && self.attempts.is_empty()).then_some(0)
            }),
        })
    }

    fn spawn_limit_cleanup(&mut self, failed: bool) {
        if let Some(cleanup) = self.take_limit_cleanup(failed) {
            tokio::spawn(cleanup.run());
        }
    }

    /// Emits the request's metadata event. The attempts move out of the
    /// guard: every caller is finishing or dropping it.
    fn emit(&mut self, outcome: &RequestOutcome, finalize_attempt: bool) {
        let mut attempts = std::mem::take(&mut self.attempts);
        if let Some(active) = self.active_attempt.take() {
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
                attempts,
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

    fn record_request_trace(&self, outcome: &RequestOutcome) {
        let Some(trace) = &self.request_trace else {
            return;
        };
        let attempt_count = self.attempts.len() + usize::from(self.active_attempt.is_some());
        trace.record_terminal(
            outcome.status_code,
            outcome.error_class.as_deref(),
            attempt_count,
            self.first_byte_ms,
            elapsed_ms(self.request_started.elapsed()),
        );
    }
}

impl Drop for RequestAccountingGuard {
    fn drop(&mut self) {
        if !self.armed {
            return;
        }
        let outcome = RequestOutcome::client_cancelled();
        self.record_terminal_traces(&outcome);
        let cleanup = self.take_limit_cleanup(true);
        self.emit(&outcome, false);
        let Some(cleanup) = cleanup else {
            return;
        };
        if let Ok(runtime) = tokio::runtime::Handle::try_current() {
            runtime.spawn(cleanup.run());
        } else {
            tracing::warn!("request limit cleanup skipped outside a Tokio runtime");
        }
    }
}

pub(in crate::inference) struct RequestMetadataFinalizer(RequestAccountingGuard);

impl RequestMetadataFinalizer {
    pub(in crate::inference) fn finalize(mut self, outcome: &RequestOutcome) {
        self.0.record_terminal_traces(outcome);
        self.0.emit(outcome, true);
    }
}

#[derive(Clone, Default)]
pub struct UsageCapture {
    observed: bool,
    complete: bool,
    /// Whether the provider stream actually reached its terminal event. A
    /// cancelled or timed-out stream carries real numbers that are simply not
    /// the final ones, so it must not be priced as exact.
    settled: bool,
    input_tokens: Option<i64>,
    output_tokens: Option<i64>,
    cached_input_tokens: Option<i64>,
    reasoning_tokens: Option<i64>,
    media_units: Option<Decimal>,
}

impl UsageCapture {
    pub(in crate::inference) fn record_trace(&self, trace: &AttemptTrace) {
        trace.record_usage(
            self.input_tokens,
            self.output_tokens,
            self.cached_input_tokens,
            self.media_units,
        );
    }

    #[must_use]
    pub fn actual_tokens(&self) -> Option<i64> {
        if !self.complete || !self.settled {
            return None;
        }
        self.input_tokens?.checked_add(self.output_tokens?)
    }

    #[must_use]
    pub const fn reasoning_tokens(&self) -> Option<i64> {
        self.reasoning_tokens
    }

    /// Marks the stream as having reached its terminal event.
    pub const fn settle(&mut self) {
        self.settled = true;
    }

    #[must_use]
    #[cfg(any(test, feature = "test-util"))]
    pub const fn is_settled(&self) -> bool {
        self.settled
    }

    pub fn observe(&mut self, event: &Event) {
        if matches!(event.kind, Kind::Done) {
            self.settled = true;
            return;
        }
        let Kind::Usage { usage } = &event.kind else {
            return;
        };
        self.observed = true;
        self.input_tokens = i64::try_from(usage.input_tokens).ok();
        self.output_tokens = i64::try_from(usage.output_tokens).ok();
        self.cached_input_tokens = usage
            .cached_input_tokens
            .and_then(|value| i64::try_from(value).ok());
        self.reasoning_tokens = usage
            .reasoning_tokens
            .and_then(|value| i64::try_from(value).ok());
        // Canonical `output_tokens` excludes `reasoning_tokens` (the two are
        // disjoint on the wire). Providers bill and rate-limit thinking as
        // generated output, so the stored and metered output count is the
        // reasoning-inclusive sum; otherwise a reasoning response would be
        // free of both charge and rate limit.
        self.output_tokens = billable_output_tokens(self.output_tokens, self.reasoning_tokens);
        self.complete = self.input_tokens.is_some()
            && self.output_tokens.is_some()
            && (usage.cached_input_tokens.is_none() || self.cached_input_tokens.is_some());
    }

    pub fn observe_openai_media_event(&mut self, event: &Event) {
        let Kind::SourceExtension { extensions } = &event.kind else {
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

pub(in crate::inference) fn usage_from_result(result: &CanonicalResult) -> UsageCapture {
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
            Some(crate::domain::canonical::events::Usage {
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
            let output = billable_output_tokens(
                i64::try_from(usage.output_tokens).ok(),
                usage
                    .reasoning_tokens
                    .and_then(|value| i64::try_from(value).ok()),
            );
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
        // A canonical result is the whole answer; there is no stream to truncate.
        settled: true,
        input_tokens,
        output_tokens,
        cached_input_tokens,
        reasoning_tokens: usage.and_then(|usage| {
            usage
                .reasoning_tokens
                .and_then(|value| i64::try_from(value).ok())
        }),
        media_units,
    }
}

struct RequestMetadataInput<'a> {
    generation_id: uuid::Uuid,
    api_key_id: uuid::Uuid,
    request_id: uuid::Uuid,
    route_slug: &'a RouteSlug,
    attempts: Vec<RequestAttemptMetadata>,
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
        // A stream that never reached its terminal event reports whatever the
        // last usage frame said, which is not the total the provider will bill
        // for the generation it kept producing. Record it as an estimate.
        let settled = update.usage.settled;
        attempt.usage = Some(RequestAttemptUsageMetadata {
            observed: true,
            complete: update.usage.complete && settled,
            billing_uncertain: !settled,
            input_tokens: update.usage.input_tokens,
            output_tokens: update.usage.output_tokens,
            cached_input_tokens: update.usage.cached_input_tokens,
            media_units: update.usage.media_units,
        });
    }
}

fn emit_request_metadata_event(service: &Service, input: RequestMetadataInput<'_>) {
    let Some(emitter) = service.request_metadata() else {
        return;
    };
    let request_completed_at = Utc::now();
    let mut attempts = input.attempts;
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
    let result = emitter.emit(MetadataEvent {
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
        usage_complete: input.usage.observed && input.usage.complete && input.usage.settled,
        unpriced: true,
        attempts,
    });
    if result.is_err() {
        error!(request_id = %input.request_id, "request metadata buffer overflowed");
    }
}

/// Output tokens as providers meter them: canonical `output_tokens` plus the
/// disjoint `reasoning_tokens`. `None` reasoning leaves the count untouched.
fn billable_output_tokens(output: Option<i64>, reasoning: Option<i64>) -> Option<i64> {
    match (output, reasoning) {
        (Some(output), Some(reasoning)) => Some(output.saturating_add(reasoning)),
        (output, _) => output,
    }
}

#[cfg(test)]
mod tests;
