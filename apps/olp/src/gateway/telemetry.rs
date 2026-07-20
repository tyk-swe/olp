use std::time::Duration;

use axum::http::StatusCode;
use chrono::Utc;
use olp_domain::{CanonicalEvent, CanonicalEventKind, CanonicalResult, RouteSlug, Surface};
use olp_storage::{RequestAttemptMetadata, RequestMetadataEvent};
use rust_decimal::{Decimal, prelude::FromPrimitive as _};
use serde_json::Value;
use tracing::error;

use crate::ApiState;

use super::{error::InferenceError, execution::RoutedEventExecution};

pub(crate) fn emit_event_execution(
    state: &ApiState,
    execution: &RoutedEventExecution,
    usage: &UsageCapture,
    failure: Option<&InferenceError>,
) {
    emit_request_event(
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
        Some(
            failure
                .map_or(StatusCode::OK, |error| error.status)
                .as_u16(),
        ),
        failure.map(|error| error.code.to_owned()),
        true,
        usage,
        execution.surface,
        execution.operation_kind.as_str(),
    );
}

pub(super) struct UnaryExecutionCompletion {
    pub(super) state: ApiState,
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
    pub(super) operation: &'static str,
}

impl UnaryExecutionCompletion {
    pub(super) fn emit(self, failure: Option<&InferenceError>) {
        emit_request_event(
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
            Some(
                failure
                    .map_or(StatusCode::OK, |error| error.status)
                    .as_u16(),
            ),
            failure.map(|error| error.code.to_owned()),
            true,
            &self.usage,
            self.surface,
            self.operation,
        );
    }
}

pub(super) fn usage_from_result(result: &CanonicalResult) -> UsageCapture {
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

#[derive(Default)]
pub(crate) struct UsageCapture {
    observed: bool,
    complete: bool,
    input_tokens: Option<i64>,
    output_tokens: Option<i64>,
    cached_input_tokens: Option<i64>,
    media_units: Option<Decimal>,
}

impl UsageCapture {
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
pub(super) fn emit_request_event(
    state: &ApiState,
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
    operation: &'static str,
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
            operation: operation
                .parse()
                .expect("request metadata uses a canonical operation"),
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
            unpriced: true,
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
