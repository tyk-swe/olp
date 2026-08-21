use olp_engine::inference::request_metadata::{Event, RequestAttemptMetadata};
use rust_decimal::Decimal;

use crate::error::Error;

pub(super) struct ValidatedAttempt<'a> {
    pub(super) event: &'a RequestAttemptMetadata,
    pub(super) usage: ValidatedAttemptUsage,
    pub(super) ordinal: i16,
    pub(super) status_code: Option<i32>,
    pub(super) latency_ms: i32,
    pub(super) first_byte_ms: Option<i32>,
}

#[derive(Clone)]
pub(super) struct ValidatedAttemptUsage {
    pub(super) observed: bool,
    pub(super) complete: bool,
    pub(super) billing_uncertain: bool,
    pub(super) input_tokens: Option<i64>,
    pub(super) output_tokens: Option<i64>,
    pub(super) cached_input_tokens: Option<i64>,
    pub(super) media_units: Option<Decimal>,
}

pub(super) struct ValidatedRequestMetadata<'a> {
    pub(super) has_attempts: bool,
    pub(super) status_code: Option<i32>,
    pub(super) latency_ms: i32,
    pub(super) first_byte_ms: Option<i32>,
    pub(super) attempt_count: i16,
    pub(super) attempts: Vec<ValidatedAttempt<'a>>,
}

impl<'a> ValidatedRequestMetadata<'a> {
    pub(super) fn validate(event: &'a Event) -> Result<Self, Error> {
        let has_attempts = !event.attempts.is_empty();
        let final_target_matches = event.attempts.last().is_none_or(|attempt| {
            event.provider_id == Some(attempt.provider_id)
                && event.upstream_model.as_deref() == Some(attempt.upstream_model.as_str())
                && attempt.committed == event.committed
        });
        let empty_attempt_metadata_is_valid = has_attempts
            || (event.provider_id.is_none()
                && event.upstream_model.is_none()
                && !event.committed
                && event.first_byte_ms.is_none()
                && event.input_tokens.is_none()
                && event.output_tokens.is_none()
                && event.cached_input_tokens.is_none()
                && event.media_units.is_none()
                && !event.usage_complete);
        let has_attempt_usage = event.attempts.iter().any(|attempt| attempt.usage.is_some());
        let attempt_usage_shape_is_valid =
            !has_attempt_usage || event.attempts.iter().all(|attempt| attempt.usage.is_some());
        if event.request_completed_at < event.request_started_at
            || event.route_slug.trim().is_empty()
            || !valid_status(event.status_code)
            || !final_target_matches
            || !empty_attempt_metadata_is_valid
            || !attempt_usage_shape_is_valid
        {
            return Err(Error::InvalidRequestMetadataEvent);
        }

        let attempts = event
            .attempts
            .iter()
            .enumerate()
            .map(|(index, attempt)| {
                if usize::from(attempt.ordinal) != index + 1
                    || attempt.completed_at < attempt.started_at
                    || !valid_status(attempt.status_code)
                {
                    return Err(Error::InvalidRequestMetadataEvent);
                }
                let usage = validated_attempt_usage(event, attempt, index)?;
                Ok(ValidatedAttempt {
                    event: attempt,
                    usage,
                    ordinal: i16::try_from(attempt.ordinal)
                        .map_err(|_| Error::InvalidRequestMetadataEvent)?,
                    status_code: attempt.status_code.map(i32::from),
                    latency_ms: checked_milliseconds(attempt.latency_ms)?,
                    first_byte_ms: attempt
                        .first_byte_ms
                        .map(checked_milliseconds)
                        .transpose()?,
                })
            })
            .collect::<Result<Vec<_>, _>>()?;

        Ok(Self {
            has_attempts,
            status_code: event.status_code.map(i32::from),
            latency_ms: checked_milliseconds(event.latency_ms)?,
            first_byte_ms: event.first_byte_ms.map(checked_milliseconds).transpose()?,
            attempt_count: i16::try_from(attempts.len())
                .map_err(|_| Error::InvalidRequestMetadataEvent)?,
            attempts,
        })
    }
}

fn validated_attempt_usage(
    request: &Event,
    attempt: &RequestAttemptMetadata,
    index: usize,
) -> Result<ValidatedAttemptUsage, Error> {
    let usage = attempt
        .usage
        .as_ref()
        .map(|usage| ValidatedAttemptUsage {
            observed: usage.observed,
            complete: usage.complete,
            billing_uncertain: usage.billing_uncertain,
            input_tokens: usage.input_tokens,
            output_tokens: usage.output_tokens,
            cached_input_tokens: usage.cached_input_tokens,
            media_units: usage.media_units,
        })
        .unwrap_or_else(|| {
            let is_final = index + 1 == request.attempts.len();
            if is_final {
                let observed = request.usage_complete
                    || request.input_tokens.is_some()
                    || request.output_tokens.is_some()
                    || request.cached_input_tokens.is_some()
                    || request.media_units.is_some();
                ValidatedAttemptUsage {
                    observed,
                    complete: request.usage_complete,
                    billing_uncertain: !observed && legacy_attempt_may_be_billable(attempt),
                    input_tokens: request.input_tokens,
                    output_tokens: request.output_tokens,
                    cached_input_tokens: request.cached_input_tokens,
                    media_units: request.media_units,
                }
            } else {
                let billing_uncertain = legacy_attempt_may_be_billable(attempt);
                ValidatedAttemptUsage {
                    observed: false,
                    complete: !billing_uncertain,
                    billing_uncertain,
                    input_tokens: None,
                    output_tokens: None,
                    cached_input_tokens: None,
                    media_units: None,
                }
            }
        });
    let state_is_valid = matches!(
        (usage.observed, usage.complete, usage.billing_uncertain),
        (false, true, false) | (false, false, true) | (true, _, false)
    );
    if !state_is_valid
        || !usage.observed
            && (usage.input_tokens.is_some()
                || usage.output_tokens.is_some()
                || usage.cached_input_tokens.is_some()
                || usage.media_units.is_some())
        || usage.input_tokens.is_some_and(|value| value < 0)
        || usage.output_tokens.is_some_and(|value| value < 0)
        || usage.cached_input_tokens.is_some_and(|value| value < 0)
        || usage.media_units.is_some_and(|value| value < Decimal::ZERO)
    {
        return Err(Error::InvalidRequestMetadataEvent);
    }
    Ok(usage)
}

fn legacy_attempt_may_be_billable(attempt: &RequestAttemptMetadata) -> bool {
    attempt.committed
        || matches!(
            attempt.error_class.as_deref(),
            Some("ambiguous" | "timeout" | "upstream_server" | "protocol" | "cancelled")
        )
}

const fn valid_status(status: Option<u16>) -> bool {
    match status {
        Some(status) => status >= 100 && status <= 599,
        None => true,
    }
}

fn checked_milliseconds(value: u64) -> Result<i32, Error> {
    i32::try_from(value).map_err(|_| Error::InvalidRequestMetadataEvent)
}

#[cfg(test)]
mod tests {
    use chrono::{Duration, Utc};
    use olp_engine::{
        domain::canonical::identity::{OperationKind, Surface},
        inference::request_metadata::{Event, RequestAttemptMetadata, RequestAttemptUsageMetadata},
    };
    use uuid::Uuid;

    use super::*;

    fn attempt(
        ordinal: u16,
        provider_id: Uuid,
        model: &str,
        committed: bool,
    ) -> RequestAttemptMetadata {
        let completed_at = Utc::now();
        RequestAttemptMetadata {
            id: Uuid::now_v7(),
            ordinal,
            provider_id,
            upstream_model: model.to_owned(),
            started_at: completed_at - Duration::milliseconds(10),
            completed_at,
            status_code: Some(200),
            error_class: None,
            committed,
            latency_ms: 10,
            first_byte_ms: Some(3),
            usage: Some(RequestAttemptUsageMetadata {
                observed: committed,
                complete: true,
                billing_uncertain: false,
                input_tokens: committed.then_some(7),
                output_tokens: None,
                cached_input_tokens: None,
                media_units: None,
            }),
        }
    }

    fn event() -> Event {
        let completed_at = Utc::now();
        let first_provider = Uuid::now_v7();
        let final_provider = Uuid::now_v7();
        Event {
            event_id: Uuid::now_v7(),
            request_id: Uuid::now_v7(),
            runtime_generation_id: Uuid::now_v7(),
            api_key_id: Uuid::now_v7(),
            provider_id: Some(final_provider),
            route_slug: "primary".to_owned(),
            upstream_model: Some("model-b".to_owned()),
            operation: OperationKind::Generation,
            surface: Surface::OpenAi,
            request_started_at: completed_at - Duration::milliseconds(25),
            request_completed_at: completed_at,
            observed_at: completed_at,
            status_code: Some(200),
            error_class: None,
            committed: true,
            latency_ms: 25,
            first_byte_ms: Some(18),
            input_tokens: Some(7),
            output_tokens: None,
            cached_input_tokens: None,
            media_units: None,
            usage_complete: true,
            unpriced: false,
            attempts: vec![
                attempt(1, first_provider, "model-a", false),
                attempt(2, final_provider, "model-b", true),
            ],
        }
    }

    type EventMutation = fn(&mut Event);

    fn assert_invalid_events(cases: &[(&str, EventMutation)]) {
        for (name, mutate) in cases {
            let mut candidate = event();
            mutate(&mut candidate);
            assert!(
                matches!(
                    ValidatedRequestMetadata::validate(&candidate),
                    Err(Error::InvalidRequestMetadataEvent)
                ),
                "accepted {name}"
            );
        }
    }

    #[test]
    fn validation_normalizes_a_well_formed_attempt_sequence() {
        let event = event();
        let validated = ValidatedRequestMetadata::validate(&event).unwrap();

        assert!(validated.has_attempts);
        assert_eq!(validated.status_code, Some(200));
        assert_eq!(validated.latency_ms, 25);
        assert_eq!(validated.first_byte_ms, Some(18));
        assert_eq!(validated.attempt_count, 2);
        assert_eq!(validated.attempts[0].ordinal, 1);
        assert_eq!(validated.attempts[1].status_code, Some(200));
        assert_eq!(validated.attempts[1].latency_ms, 10);
        assert_eq!(validated.attempts[1].first_byte_ms, Some(3));
        assert_eq!(validated.attempts[1].event.id, event.attempts[1].id);
        assert!(validated.attempts[1].usage.observed);
        assert_eq!(validated.attempts[1].usage.input_tokens, Some(7));
    }

    #[test]
    fn validation_rejects_malformed_envelopes_and_attempts() {
        assert_invalid_events(&[
            ("reversed request timing", |event| {
                event.request_completed_at = event.request_started_at - Duration::nanoseconds(1)
            }),
            ("blank route", |event| event.route_slug = "  ".to_owned()),
            ("request status outside HTTP range", |event| {
                event.status_code = Some(99)
            }),
            ("final provider mismatch", |event| {
                event.provider_id = Some(Uuid::now_v7())
            }),
            ("target metadata without an attempt", |event| {
                event.attempts.clear()
            }),
            ("mixed legacy and attempt-local usage", |event| {
                event.attempts[0].usage = None
            }),
            ("non-contiguous ordinal", |event| {
                event.attempts[0].ordinal = 2
            }),
            ("reversed attempt timing", |event| {
                event.attempts[0].completed_at =
                    event.attempts[0].started_at - Duration::nanoseconds(1)
            }),
            ("attempt status outside HTTP range", |event| {
                event.attempts[0].status_code = Some(600)
            }),
            ("request latency overflow", |event| {
                event.latency_ms = i32::MAX as u64 + 1
            }),
            ("attempt first-byte overflow", |event| {
                event.attempts[0].first_byte_ms = Some(i32::MAX as u64 + 1)
            }),
        ]);
    }

    #[test]
    fn attempt_usage_state_machine_rejects_inconsistent_or_negative_evidence() {
        assert_invalid_events(&[
            ("missing evidence without billing uncertainty", |event| {
                event.attempts[0].usage.as_mut().unwrap().complete = false
            }),
            ("complete and billing-uncertain", |event| {
                let usage = event.attempts[0].usage.as_mut().unwrap();
                usage.billing_uncertain = true;
                usage.complete = true;
            }),
            ("tokens without observed usage", |event| {
                event.attempts[0].usage.as_mut().unwrap().input_tokens = Some(1)
            }),
            ("negative token count", |event| {
                event.attempts[1].usage.as_mut().unwrap().output_tokens = Some(-1)
            }),
            ("negative media units", |event| {
                event.attempts[1].usage.as_mut().unwrap().media_units = Some(Decimal::NEGATIVE_ONE)
            }),
        ]);
    }

    #[test]
    fn legacy_attempts_derive_conservative_billing_evidence() {
        let mut event = event();
        for attempt in &mut event.attempts {
            attempt.usage = None;
        }
        event.attempts[0].error_class = Some("timeout".to_owned());

        let validated = ValidatedRequestMetadata::validate(&event).unwrap();
        let first = &validated.attempts[0].usage;
        assert!(!first.observed);
        assert!(!first.complete);
        assert!(first.billing_uncertain);
        assert!(validated.attempts[1].usage.observed);
        assert!(validated.attempts[1].usage.complete);
        assert!(!validated.attempts[1].usage.billing_uncertain);

        for (error, committed, billable) in [
            (None, false, false),
            (Some("connect"), false, false),
            (Some("ambiguous"), false, true),
            (Some("timeout"), false, true),
            (Some("upstream_server"), false, true),
            (Some("protocol"), false, true),
            (Some("cancelled"), false, true),
            (None, true, true),
        ] {
            let mut candidate = event.attempts[0].clone();
            candidate.error_class = error.map(str::to_owned);
            candidate.committed = committed;
            assert_eq!(
                legacy_attempt_may_be_billable(&candidate),
                billable,
                "error={error:?}, committed={committed}"
            );
        }
    }

    #[test]
    fn empty_pre_attempt_events_are_valid_only_without_usage_or_target_metadata() {
        let mut event = event();
        event.attempts.clear();
        event.provider_id = None;
        event.upstream_model = None;
        event.committed = false;
        event.first_byte_ms = None;
        event.input_tokens = None;
        event.output_tokens = None;
        event.cached_input_tokens = None;
        event.media_units = None;
        event.usage_complete = false;

        let validated = ValidatedRequestMetadata::validate(&event).unwrap();
        assert!(!validated.has_attempts);
        assert_eq!(validated.attempt_count, 0);
    }
}
