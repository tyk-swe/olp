use std::io;

use super::lifecycle::{
    AttachmentErrorClass, allows_refresh_transition, classify_attachment_error,
    classify_attachment_sqlstate, validate_progress, validate_update,
};
use super::*;

#[test]
fn attachment_sqlstate_classifier_is_operation_specific() {
    use AttachmentErrorClass::{AmbiguousCompletion, DefiniteTransient, Permanent};

    for (code, expected) in [
        (Some("40001"), DefiniteTransient),
        (Some("40P01"), DefiniteTransient),
        (Some("55P03"), DefiniteTransient),
        (Some("57014"), DefiniteTransient),
        (Some("57P01"), DefiniteTransient),
        (Some("08006"), AmbiguousCompletion),
        (Some("40003"), AmbiguousCompletion),
        (Some("P0001"), Permanent),
        (Some("23505"), Permanent),
        (Some("42P01"), Permanent),
        (Some("22P02"), Permanent),
        (None, Permanent),
    ] {
        assert_eq!(classify_attachment_sqlstate(code), expected, "{code:?}");
    }
}

#[test]
fn attachment_sqlx_error_classifier_keeps_decode_and_configuration_permanent() {
    use AttachmentErrorClass::{AmbiguousCompletion, DefiniteTransient, Permanent};

    let cases = [
        (
            sqlx::Error::Io(io::Error::new(io::ErrorKind::ConnectionReset, "reset")),
            AmbiguousCompletion,
        ),
        (
            sqlx::Error::Protocol("invalid response".to_owned()),
            AmbiguousCompletion,
        ),
        (sqlx::Error::WorkerCrashed, AmbiguousCompletion),
        (sqlx::Error::PoolTimedOut, DefiniteTransient),
        (sqlx::Error::PoolClosed, Permanent),
        (sqlx::Error::decode("invalid column value"), Permanent),
        (
            sqlx::Error::config(io::Error::other("invalid URL")),
            Permanent,
        ),
        (
            sqlx::Error::ColumnNotFound("lifecycle_state".to_owned()),
            Permanent,
        ),
    ];
    for (error, expected) in cases {
        assert_eq!(classify_attachment_error(&error), expected, "{error}");
    }
}

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
