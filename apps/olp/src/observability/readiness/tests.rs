use std::{path::PathBuf, sync::Arc};

use olp_db::{
    request_metadata::delivery_health::{ConsumerHealth, ConsumerStatus},
    runtime::outbox::{RuntimeOutboxState, RuntimeOutboxStatus},
    worker_health::{WorkerTaskState, WorkerTaskStatus},
};
use olp_engine::inference::runtime::Manager;

use super::*;
use crate::{application::mode::ApiMode, bootstrap::state::ProcessComposition};

#[test]
fn http_only_processes_still_check_fleet_worker_summaries() {
    let now = chrono::DateTime::parse_from_rfc3339("2026-08-08T12:00:00Z")
        .unwrap()
        .with_timezone(&chrono::Utc);
    let stale_consumer = ConsumerStatus::from_health(
        Some(ConsumerHealth {
            pending_events: 0,
            lag_events: 0,
            oldest_pending_at: None,
            checked_at: now - chrono::Duration::seconds(21),
        }),
        now,
    );
    let healthy_outbox = RuntimeOutboxStatus {
        state: RuntimeOutboxState::Healthy,
        pending_rows: 0,
        oldest_pending_at: None,
        owner_active: true,
        claimed_rows: 0,
        checked_at: Some(now),
        heartbeat_age_seconds: Some(0),
        last_progress_at: Some(now),
        last_progress_age_seconds: Some(0),
    };
    let healthy_tasks = WorkerTaskHealthSummary {
        tasks: WorkerTask::ALL
            .into_iter()
            .map(|task| WorkerTaskStatus {
                task,
                state: WorkerTaskState::Healthy,
                checked_at: Some(now),
                last_success_at: Some(now),
                last_progress_at: Some(now),
                heartbeat_age_seconds: Some(0),
                last_success_age_seconds: Some(0),
                successes_total: 1,
                failures_total: 0,
                skipped_total: 0,
            })
            .collect(),
    };

    for mode in [ApiMode::Gateway, ApiMode::Control] {
        let state = ProcessComposition::new(
            mode,
            crate::bootstrap::mode_dependencies::test_store(),
            Arc::new(Manager::empty()),
            "https://olp.example.test",
            PathBuf::from("missing-console"),
        )
        .observability_state_for_test();
        state.limiter().mark_configured();

        let expected_tasks = expected_worker_tasks(&state);
        assert_eq!(expected_tasks, WorkerTask::ALL.as_slice());
        let (current, drained) = asynchronous_plane_flags(
            &healthy_tasks,
            expected_tasks,
            stale_consumer,
            healthy_outbox,
        );

        assert!(!current, "{mode} must not skip the stale durable consumer");
        assert!(drained, "{mode} should separate staleness from backlog");
        assert_eq!(
            asynchronous_plane_state(
                current,
                drained,
                &healthy_tasks,
                expected_tasks,
                stale_consumer,
                healthy_outbox,
            ),
            "stale"
        );
    }
}
