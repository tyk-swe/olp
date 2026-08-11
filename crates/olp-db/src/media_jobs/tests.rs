use super::lifecycle::{allows_refresh_transition, validate_progress, validate_update};
use super::*;

#[test]
fn rejects_nonfinite_progress_and_inconsistent_result_state() {
    assert!(validate_progress(Some(f32::NAN)).is_err());
    assert!(validate_progress(Some(100.1)).is_err());
    assert!(
        validate_update(&MediaJobUpdate {
            state: MediaJobState::Running,
            progress_percent: Some(50.0),
            content_available: true,
            expires_at: None,
            error_class: None,
            last_polled_at: Utc::now(),
        })
        .is_err()
    );
}

#[test]
fn refresh_transitions_never_regress_or_change_terminal_outcomes() {
    assert!(allows_refresh_transition(
        MediaJobState::Queued,
        MediaJobState::Succeeded
    ));
    assert!(!allows_refresh_transition(
        MediaJobState::Running,
        MediaJobState::Queued
    ));
    assert!(!allows_refresh_transition(
        MediaJobState::Succeeded,
        MediaJobState::Running
    ));
    assert!(!allows_refresh_transition(
        MediaJobState::Failed,
        MediaJobState::Cancelled
    ));
    assert!(MediaJobLifecycle::Creating.needs_reconciliation());
    assert!(MediaJobLifecycle::DeletePending.needs_reconciliation());
    assert!(!MediaJobLifecycle::Active.needs_reconciliation());
    assert!(!MediaJobLifecycle::Deleted.needs_reconciliation());
}
