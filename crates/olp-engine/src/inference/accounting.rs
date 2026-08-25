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
        if let (Some(reservation), Some(actual)) = (self.admission_reservation, admission_actual) {
            reservation.reconcile(actual).await;
        }
        release(self.delta_lease, delta_actual).await;
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
    ) {
        self.attempts = attempts;
        self.active_attempt = None;
        self.attempt_started = attempt_started;
        self.first_byte_ms = first_byte_ms;
        self.committed = committed;
    }

    #[must_use]
    pub fn usage_mut(&mut self) -> &mut UsageCapture {
        &mut self.usage
    }

    pub(in crate::inference) fn replace_usage(&mut self, usage: UsageCapture) {
        self.usage = usage;
    }

    pub async fn release(&mut self) {
        self.release_failed(false).await;
    }

    async fn release_failed(&mut self, failed: bool) {
        let Some(task) = self.spawn_limit_cleanup(failed) else {
            return;
        };
        if let Err(error) = task.await {
            tracing::warn!(%error, "request limit cleanup task failed");
        }
    }

    pub(in crate::inference) fn release_detached(&mut self) {
        let _ = self.spawn_limit_cleanup(false);
    }

    pub async fn finish(mut self, outcome: RequestOutcome) {
        self.release_failed(outcome.error_class.is_some()).await;
        self.emit(&outcome, true);
        self.armed = false;
    }

    pub(in crate::inference) fn finish_detached(mut self, outcome: RequestOutcome) {
        let _ = self.spawn_limit_cleanup(outcome.error_class.is_some());
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
            // A request that failed consumed no tokens we can attribute, so it
            // must give the conservative reservation back. Leaving it charged
            // let a run of upstream 502s eat a whole minute's TPM budget and
            // then 429 unrelated, valid traffic. A *successful* request whose
            // provider reported nothing keeps the estimate: it really did use
            // tokens, we just never learned how many.
            actual_tokens: self.usage.actual_tokens().or(failed.then_some(0)),
        })
    }

    fn spawn_limit_cleanup(&mut self, failed: bool) -> Option<tokio::task::JoinHandle<()>> {
        Some(tokio::spawn(self.take_limit_cleanup(failed)?.run()))
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

impl Drop for RequestAccountingGuard {
    fn drop(&mut self) {
        if !self.armed {
            return;
        }
        self.emit(&RequestOutcome::client_cancelled(), false);
        let Some(cleanup) = self.take_limit_cleanup(true) else {
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
    pub(in crate::inference) fn finalize(self, outcome: &RequestOutcome) {
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
    #[must_use]
    pub fn actual_tokens(&self) -> Option<i64> {
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
mod tests {
    use std::sync::{
        Arc, Mutex,
        atomic::{AtomicUsize, Ordering},
    };

    use super::{
        FinalAttemptUpdate, LimitCleanup, RequestAttemptMetadata, RequestAttemptUsageMetadata,
        RequestOutcome, UsageCapture, billable_output_tokens, split_actual_tokens,
        update_final_attempt,
    };
    use crate::{
        domain::ports::BoxFuture,
        inference::limits::{LimitError, LimitLease, Reservation},
    };
    use chrono::Utc;
    use uuid::Uuid;

    #[derive(Default)]
    struct CleanupEffects {
        reconciled_tokens: Mutex<Vec<i64>>,
        releases: AtomicUsize,
    }

    struct RecordingLease {
        effects: Arc<CleanupEffects>,
    }

    impl LimitLease for RecordingLease {
        fn reconcile(&self, actual_tokens: i64) -> BoxFuture<'_, Result<(), LimitError>> {
            self.effects
                .reconciled_tokens
                .lock()
                .unwrap()
                .push(actual_tokens);
            Box::pin(async { Ok(()) })
        }

        fn release(&self) -> BoxFuture<'_, Result<(), LimitError>> {
            self.effects.releases.fetch_add(1, Ordering::Relaxed);
            Box::pin(async { Ok(()) })
        }
    }

    fn recording_lease(effects: &Arc<CleanupEffects>) -> Arc<dyn LimitLease> {
        Arc::new(RecordingLease {
            effects: Arc::clone(effects),
        })
    }

    async fn assert_cleanup_effects(
        actual_tokens: Option<i64>,
        expected_admission_tokens: &[i64],
        expected_delta_tokens: &[i64],
    ) {
        let admission = Arc::new(CleanupEffects::default());
        let delta = Arc::new(CleanupEffects::default());
        let admission_reservation = Reservation::distributed(recording_lease(&admission));
        let repeated_cleanup = admission_reservation.clone();
        LimitCleanup {
            delta_lease: Some(Reservation::distributed(recording_lease(&delta))),
            admission_reservation: Some(admission_reservation.clone()),
            admission_reserved_tokens: Some(100),
            actual_tokens,
        }
        .run()
        .await;

        if actual_tokens.is_some() {
            repeated_cleanup.reconcile(i64::MAX).await;
        }
        repeated_cleanup.release().await;
        admission_reservation.release().await;

        assert_eq!(
            admission.reconciled_tokens.lock().unwrap().as_slice(),
            expected_admission_tokens
        );
        assert_eq!(admission.releases.load(Ordering::Relaxed), 1);
        assert_eq!(
            delta.reconciled_tokens.lock().unwrap().as_slice(),
            expected_delta_tokens
        );
        assert_eq!(delta.releases.load(Ordering::Relaxed), 1);
    }

    #[test]
    fn successful_outcome_preserves_the_http_status() {
        let outcome = RequestOutcome::success_with_status(201);
        assert_eq!(outcome.status_code, Some(201));
        assert_eq!(outcome.error_class, None);
    }

    #[test]
    fn actual_tokens_are_split_across_admission_and_delta_reservations() {
        assert_eq!(
            split_actual_tokens(Some(40), Some(100), true, true),
            (Some(40), Some(0))
        );
        assert_eq!(
            split_actual_tokens(Some(130), Some(100), true, true),
            (Some(100), Some(30))
        );
        assert_eq!(
            split_actual_tokens(Some(40), None, false, true),
            (None, Some(40))
        );
        assert_eq!(
            split_actual_tokens(Some(130), Some(100), true, false),
            (Some(130), None)
        );
        assert_eq!(
            split_actual_tokens(None, Some(100), true, true),
            (None, None)
        );
    }

    #[tokio::test]
    async fn cleanup_reconciles_both_reservations_to_final_usage_once() {
        for (actual, admission, delta) in [
            (40, vec![40], vec![0]),
            (100, vec![100], vec![0]),
            (130, vec![100], vec![30]),
        ] {
            assert_cleanup_effects(Some(actual), &admission, &delta).await;
        }
    }

    #[tokio::test]
    async fn cleanup_without_actual_usage_only_releases_both_reservations() {
        assert_cleanup_effects(None, &[], &[]).await;
    }

    #[tokio::test]
    async fn cleanup_reconciles_full_usage_against_the_only_existing_reservation() {
        let admission = Arc::new(CleanupEffects::default());
        let admission_reservation = Reservation::distributed(recording_lease(&admission));
        let admission_release = admission_reservation.clone();
        LimitCleanup {
            delta_lease: None,
            admission_reservation: Some(admission_reservation),
            admission_reserved_tokens: Some(100),
            actual_tokens: Some(130),
        }
        .run()
        .await;
        assert_eq!(
            admission.reconciled_tokens.lock().unwrap().as_slice(),
            &[130]
        );
        admission_release.release().await;

        let delta = Arc::new(CleanupEffects::default());
        LimitCleanup {
            delta_lease: Some(Reservation::distributed(recording_lease(&delta))),
            admission_reservation: None,
            admission_reserved_tokens: None,
            actual_tokens: Some(40),
        }
        .run()
        .await;
        assert_eq!(delta.reconciled_tokens.lock().unwrap().as_slice(), &[40]);
    }

    #[test]
    fn final_streaming_usage_is_attached_to_the_final_attempt() {
        let mut attempt = uncertain_attempt();
        let usage = UsageCapture {
            observed: true,
            complete: true,
            settled: true,
            input_tokens: Some(12),
            output_tokens: Some(7),
            cached_input_tokens: Some(2),
            reasoning_tokens: None,
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

    #[test]
    fn a_stream_that_never_reached_done_is_priced_as_an_estimate() {
        let mut attempt = uncertain_attempt();
        let error = Some("client_cancelled".to_owned());
        let usage = UsageCapture {
            observed: true,
            complete: true,
            settled: false,
            input_tokens: Some(12),
            output_tokens: Some(7),
            cached_input_tokens: None,
            reasoning_tokens: None,
            media_units: None,
        };
        update_final_attempt(
            &mut attempt,
            FinalAttemptUpdate {
                completed_at: Utc::now(),
                started: tokio::time::Instant::now(),
                first_byte_ms: Some(3),
                status_code: None,
                error_class: &error,
                committed: true,
                usage: &usage,
            },
        );
        let attempt_usage = attempt.usage.unwrap();
        assert!(attempt_usage.observed);
        assert!(
            attempt_usage.billing_uncertain,
            "an aborted stream cannot be recorded as an exact charge"
        );
        assert!(!attempt_usage.complete);
        assert_eq!(attempt_usage.output_tokens, Some(7));
    }

    #[test]
    fn a_terminal_done_event_settles_the_capture() {
        use crate::domain::canonical::events::{Event, Kind, Usage};

        let mut usage = UsageCapture::default();
        assert!(!usage.is_settled());
        usage.observe(&Event::new(
            0,
            Kind::Usage {
                usage: Usage {
                    input_tokens: 10,
                    output_tokens: 5,
                    total_tokens: 15,
                    cached_input_tokens: None,
                    reasoning_tokens: None,
                },
            },
        ));
        assert!(!usage.is_settled());
        usage.observe(&Event::new(1, Kind::Done));
        assert!(usage.is_settled());
    }

    #[test]
    fn reasoning_tokens_are_metered_as_billable_output() {
        use crate::domain::canonical::events::{Event, Kind, Usage};

        // Canonical usage keeps reasoning disjoint from output; accounting
        // meters the provider-billed sum so thinking is neither free of
        // charge nor of the TPM limit.
        let mut usage = UsageCapture::default();
        usage.observe(&Event::new(
            0,
            Kind::Usage {
                usage: Usage {
                    input_tokens: 10,
                    output_tokens: 5,
                    total_tokens: 135,
                    cached_input_tokens: None,
                    reasoning_tokens: Some(120),
                },
            },
        ));
        assert_eq!(usage.reasoning_tokens(), Some(120));
        assert_eq!(usage.output_tokens, Some(125));
        assert_eq!(usage.actual_tokens(), Some(135));

        // No reasoning reported: the output count is used as-is.
        let mut plain = UsageCapture::default();
        plain.observe(&Event::new(
            0,
            Kind::Usage {
                usage: Usage {
                    input_tokens: 10,
                    output_tokens: 5,
                    total_tokens: 15,
                    cached_input_tokens: None,
                    reasoning_tokens: None,
                },
            },
        ));
        assert_eq!(plain.actual_tokens(), Some(15));
        assert_eq!(billable_output_tokens(None, Some(3)), None);
    }

    #[tokio::test]
    async fn a_failed_request_without_usage_refunds_its_whole_reservation() {
        let admission = Arc::new(CleanupEffects::default());
        let admission_reservation = Reservation::distributed(recording_lease(&admission));
        let release = admission_reservation.clone();
        LimitCleanup {
            delta_lease: None,
            admission_reservation: Some(admission_reservation),
            admission_reserved_tokens: Some(4_096),
            // What `take_limit_cleanup(true)` produces with nothing observed.
            actual_tokens: Some(0),
        }
        .run()
        .await;
        assert_eq!(admission.reconciled_tokens.lock().unwrap().as_slice(), &[0]);
        release.release().await;
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
