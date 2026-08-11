use super::lifecycle::{allows_refresh_transition, validate_progress, validate_update};
use super::*;

#[test]
fn progress_is_a_finite_closed_percentage() {
    for valid in [None, Some(0.0), Some(50.0), Some(100.0)] {
        assert!(validate_progress(valid).is_ok(), "rejected {valid:?}");
    }
    for invalid in [-0.1, 100.1, f32::NAN] {
        assert!(
            validate_progress(Some(invalid)).is_err(),
            "accepted {invalid:?}"
        );
    }
}

#[test]
fn update_payloads_bind_content_and_errors_to_terminal_states() {
    for (state, content_available, error_class, valid) in [
        (MediaJobState::Running, false, None, true),
        (MediaJobState::Succeeded, true, None, true),
        (MediaJobState::Failed, false, Some("upstream"), true),
        (MediaJobState::Running, true, None, false),
        (MediaJobState::Succeeded, false, Some("upstream"), false),
    ] {
        let result = validate_update(&MediaJobUpdate {
            state,
            progress_percent: Some(50.0),
            content_available,
            expires_at: None,
            error_class: error_class.map(str::to_owned),
            last_polled_at: Utc::now(),
        });
        assert_eq!(
            result.is_ok(),
            valid,
            "unexpected validation for {state:?}, content={content_available}, error={error_class:?}"
        );
    }
}

#[test]
fn refresh_transition_matrix_never_regresses_or_changes_terminal_outcomes() {
    const STATES: [MediaJobState; 5] = [
        MediaJobState::Queued,
        MediaJobState::Running,
        MediaJobState::Succeeded,
        MediaJobState::Failed,
        MediaJobState::Cancelled,
    ];
    for current in STATES {
        let allowed: &[MediaJobState] = match current {
            MediaJobState::Queued => &STATES,
            MediaJobState::Running => &STATES[1..],
            _ => std::slice::from_ref(&current),
        };
        for incoming in STATES {
            assert_eq!(
                allows_refresh_transition(current, incoming),
                allowed.contains(&incoming),
                "unexpected {current:?} -> {incoming:?} decision"
            );
        }
    }
}

#[test]
fn lifecycle_strings_and_reconciliation_inventory_are_complete() {
    for (lifecycle, stored, needs_reconciliation) in [
        (MediaJobLifecycle::Creating, "creating", true),
        (MediaJobLifecycle::Active, "active", false),
        (MediaJobLifecycle::CreateAmbiguous, "create_ambiguous", true),
        (
            MediaJobLifecycle::CreateCleanupPending,
            "create_cleanup_pending",
            true,
        ),
        (MediaJobLifecycle::DeletePending, "delete_pending", true),
        (MediaJobLifecycle::Deleted, "deleted", false),
    ] {
        assert_eq!(lifecycle.as_str(), stored);
        assert_eq!(MediaJobLifecycle::parse(stored).unwrap(), lifecycle);
        assert_eq!(lifecycle.needs_reconciliation(), needs_reconciliation);
    }
    assert!(MediaJobLifecycle::parse("unknown").is_err());
}
