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
