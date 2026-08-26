use crate::domain::{
    canonical::events::{Error, ErrorClass},
    ids::{DurationMs, ProviderId, RouteId, RuntimeGenerationId, TargetId},
    ports::{AttemptFailureClass, TransportError, TransportPhase},
    routing::{provider::ProviderKind, selection::AttemptPlan},
};
use chrono::Utc;

use super::{
    AttemptRecord, BASE_RETRY_BACKOFF, FailureHistory, MAX_RETRY_AFTER_DELAY, MAX_RETRY_BACKOFF,
    RetryPlan, attempt_billing_is_uncertain, jitter_fraction, plan_retry, retry_backoff,
    with_sole_target_retry,
};
use crate::inference::{circuit::Breaker, error::Kind as InferenceErrorKind};
use std::num::NonZeroU16;
use std::time::Duration;

fn plan(target_id: TargetId) -> AttemptPlan {
    AttemptPlan {
        generation_id: RuntimeGenerationId::new(),
        route_id: RouteId::new(),
        target_id,
        routing_id: target_id,
        provider_id: ProviderId::new(),
        provider_kind: ProviderKind::OpenAi,
        upstream_model: "backoff-test".to_owned(),
        timeout: DurationMs::new(1_000),
        priority: 0,
    }
}

#[test]
fn retry_backoff_grows_with_jitter_and_is_floored_by_the_provider_hint() {
    // Full jitter keeps the delay inside [half, whole] of the exponential.
    for retry_index in 0..4 {
        let exponential = BASE_RETRY_BACKOFF * (1 << retry_index);
        let low = retry_backoff(retry_index, None, 0.0);
        let high = retry_backoff(retry_index, None, 1.0);
        assert_eq!(low, exponential / 2);
        assert_eq!(high, exponential);
    }
    // Bounded however many attempts a route configures.
    assert_eq!(retry_backoff(30, None, 1.0), MAX_RETRY_BACKOFF);
    // A provider that named its own recovery time wins over our guess.
    assert_eq!(
        retry_backoff(0, Some(Duration::from_secs(60)), 1.0),
        Duration::from_secs(60)
    );
    // ...but never shortens the computed backoff.
    assert_eq!(
        retry_backoff(0, Some(Duration::ZERO), 1.0),
        BASE_RETRY_BACKOFF
    );
}

#[test]
fn jitter_stays_in_range_and_does_not_repeat_itself() {
    let samples = (0..64).map(|_| jitter_fraction()).collect::<Vec<_>>();
    assert!(samples.iter().all(|value| (0.0..1.0).contains(value)));
    assert!(
        samples.windows(2).any(|pair| pair[0] != pair[1]),
        "backoff jitter must decorrelate concurrent retries"
    );
}

#[test]
fn a_sole_eligible_target_earns_one_retry_inside_the_attempt_budget() {
    let two = NonZeroU16::new(2).unwrap();
    let target = TargetId::new();
    let single = with_sole_target_retry(vec![plan(target)], two);
    assert_eq!(single.len(), 2);
    assert_eq!(single[0].target_id, single[1].target_id);

    // The operator asked for exactly one attempt.
    let one = NonZeroU16::new(1).unwrap();
    assert_eq!(with_sole_target_retry(vec![plan(target)], one).len(), 1);

    // A route that already has somewhere else to go is left alone.
    let pair = with_sole_target_retry(vec![plan(TargetId::new()), plan(TargetId::new())], two);
    assert_eq!(pair.len(), 2);
    assert!(with_sole_target_retry(Vec::new(), two).is_empty());
}

fn history_after(
    phase: TransportPhase,
    class: AttemptFailureClass,
    hint: Option<Duration>,
) -> FailureHistory {
    let mut history = FailureHistory::default();
    let mut error = failure(phase, class);
    error.upstream = crate::domain::ports::UpstreamSignal::from_status(429).with_retry_after(hint);
    history.record_retry(error, None, 1);
    history
}

#[test]
fn a_same_target_retry_is_refused_once_the_attempt_may_have_been_billed() {
    assert!(!FailureHistory::default().permits_same_target_retry());
    assert!(
        history_after(
            TransportPhase::FirstByte,
            AttemptFailureClass::RateLimit,
            None
        )
        .permits_same_target_retry()
    );
    assert!(
        history_after(TransportPhase::Connect, AttemptFailureClass::Connect, None)
            .permits_same_target_retry()
    );
    assert!(
        !history_after(
            TransportPhase::FirstByte,
            AttemptFailureClass::Timeout,
            None
        )
        .permits_same_target_retry()
    );
    assert!(
        !history_after(
            TransportPhase::FirstByte,
            AttemptFailureClass::UpstreamServer,
            None
        )
        .permits_same_target_retry()
    );
}

#[tokio::test(start_paused = true)]
async fn a_retry_after_hint_is_capped_and_applies_only_to_the_same_target() {
    let far_deadline = tokio::time::Instant::now() + Duration::from_secs(600);
    let hint = Some(Duration::from_secs(120));
    let throttled = history_after(
        TransportPhase::FirstByte,
        AttemptFailureClass::RateLimit,
        hint,
    );
    let target = plan(TargetId::new());

    match plan_retry(&target, &target, 0, &throttled, far_deadline) {
        RetryPlan::Proceed(delay) => assert_eq!(delay, MAX_RETRY_AFTER_DELAY),
        RetryPlan::Stop => panic!("a rate-limited target may be retried"),
    }
    match plan_retry(&target, &plan(TargetId::new()), 0, &throttled, far_deadline) {
        RetryPlan::Proceed(delay) => assert!(delay <= BASE_RETRY_BACKOFF, "{delay:?}"),
        RetryPlan::Stop => panic!("another target is not bound by the hint"),
    }

    // Billing uncertainty stops a same-target retry but not failover.
    let timed_out = history_after(
        TransportPhase::FirstByte,
        AttemptFailureClass::Timeout,
        None,
    );
    assert!(matches!(
        plan_retry(&target, &target, 0, &timed_out, far_deadline),
        RetryPlan::Stop
    ));
    assert!(matches!(
        plan_retry(&target, &plan(TargetId::new()), 0, &timed_out, far_deadline),
        RetryPlan::Proceed(_)
    ));

    // No room before the deadline: stop rather than sleep into a timeout.
    let near_deadline = tokio::time::Instant::now() + Duration::from_millis(10);
    assert!(matches!(
        plan_retry(
            &target,
            &plan(TargetId::new()),
            0,
            &throttled,
            near_deadline
        ),
        RetryPlan::Stop
    ));
}

#[test]
fn the_provider_retry_after_survives_into_the_retry_decision() {
    let mut history = FailureHistory::default();
    assert_eq!(history.retry_after(), None);
    history.record_retry(
        TransportError {
            phase: TransportPhase::FirstByte,
            class: AttemptFailureClass::RateLimit,
            response_committed: false,
            message: "throttled".to_owned(),
            upstream: crate::domain::ports::UpstreamSignal::from_status(429)
                .with_retry_after(Some(Duration::from_secs(12))),
        },
        None,
        1,
    );
    assert_eq!(history.retry_after(), Some(Duration::from_secs(12)));
}

#[test]
fn billing_uncertainty_starts_after_a_request_may_reach_the_provider() {
    assert!(!attempt_billing_is_uncertain(&failure(
        TransportPhase::Connect,
        AttemptFailureClass::Connect,
    )));
    assert!(!attempt_billing_is_uncertain(&failure(
        TransportPhase::FirstByte,
        AttemptFailureClass::RateLimit,
    )));
    assert!(attempt_billing_is_uncertain(&failure(
        TransportPhase::FirstByte,
        AttemptFailureClass::UpstreamServer,
    )));
    assert!(attempt_billing_is_uncertain(&failure(
        TransportPhase::Body,
        AttemptFailureClass::Protocol,
    )));
    assert!(attempt_billing_is_uncertain(&failure(
        TransportPhase::FirstByte,
        AttemptFailureClass::Timeout,
    )));
}

#[test]
fn elapsed_deadline_records_attempt_without_penalizing_closed_circuit() {
    let target_id = TargetId::new();
    let attempt = AttemptPlan {
        generation_id: RuntimeGenerationId::new(),
        route_id: RouteId::new(),
        target_id,
        routing_id: target_id,
        provider_id: ProviderId::new(),
        provider_kind: ProviderKind::OpenAi,
        upstream_model: "deadline-test".to_owned(),
        timeout: DurationMs::new(1_000),
        priority: 0,
    };
    let circuits = Breaker::default();
    let record = AttemptRecord {
        plan: &attempt,
        circuit_permit: circuits
            .try_acquire_permit(target_id)
            .expect("closed circuit admits an attempt"),
        ordinal: 1,
        started_at: Utc::now(),
        started: tokio::time::Instant::now(),
    };

    let mut traces = Vec::new();
    let failure = record.record_deadline_elapsed(&mut traces, &circuits);

    assert_eq!(failure.error.code(), "gateway_timeout");
    assert_eq!(failure.attempts.len(), 1);
    let failed_attempt = &failure.attempts[0];
    assert_eq!(failed_attempt.ordinal, 1);
    assert_eq!(failed_attempt.error_class.as_deref(), Some("timeout"));
    assert_eq!(failed_attempt.status_code, Some(504));
    assert!(!failed_attempt.committed);
    let usage = failed_attempt
        .usage
        .as_ref()
        .expect("timeout attempt records billing certainty");
    assert!(usage.complete);
    assert!(!usage.billing_uncertain);
    assert_eq!(circuits.open_count(), 0);
    for _ in 0..4 {
        circuits.record_failure(target_id, AttemptFailureClass::Connect);
    }
    assert_eq!(circuits.open_count(), 0);
    assert!(circuits.is_selectable(target_id));
    circuits.record_failure(target_id, AttemptFailureClass::Connect);
    assert_eq!(circuits.open_count(), 1);
}

#[test]
fn final_retryable_canonical_error_is_preserved() {
    let mut failures = FailureHistory::default();
    failures.record_retry(
        failure(TransportPhase::FirstByte, AttemptFailureClass::RateLimit),
        Some(Error {
            class: ErrorClass::RateLimit,
            message: "provider asked the client to retry".to_owned(),
            provider_code: Some("busy".to_owned()),
            retryable: true,
        }),
        1,
    );

    let error = failures.into_error(1);

    assert_eq!(
        error.kind(),
        InferenceErrorKind::Canonical(ErrorClass::RateLimit)
    );
    assert_eq!(error.message(), "provider asked the client to retry");
}

#[test]
fn later_transport_failure_supersedes_a_canonical_error() {
    let mut failures = FailureHistory::default();
    failures.record_retry(
        failure(TransportPhase::FirstByte, AttemptFailureClass::RateLimit),
        Some(Error {
            class: ErrorClass::RateLimit,
            message: "first failure".to_owned(),
            provider_code: None,
            retryable: true,
        }),
        1,
    );
    failures.record_retry(
        failure(TransportPhase::Connect, AttemptFailureClass::Connect),
        None,
        2,
    );

    let error = failures.into_error(2);

    assert_eq!(error.code(), "upstream_unavailable");
}

fn failure(phase: TransportPhase, class: AttemptFailureClass) -> TransportError {
    TransportError {
        upstream: Default::default(),
        phase,
        class,
        response_committed: false,
        message: "metadata-free fixture".to_owned(),
    }
}

mod execute {
    use std::{
        collections::{BTreeMap, VecDeque},
        num::NonZeroU16,
        sync::{
            Arc, Mutex,
            atomic::{AtomicUsize, Ordering},
        },
        time::Duration,
    };

    use chrono::Utc;
    use futures::{future::BoxFuture, stream};

    use super::{failure, plan};
    use crate::domain::{
        canonical::{
            events::{Event, FinishReason, Kind},
            identity::{OperationKind, RequestMetadata, Surface, TransportMode},
            requests::{
                ContentPart, GenerationParameters, GenerationRequest, MediaHandle, Message,
                MessageRole, Operation, SourceExtensions,
            },
            results::MediaArtifact,
        },
        ids::{ProviderId, RequestId, RouteSlug, RuntimeGenerationId, TargetId},
        ports::{
            AttemptFailureClass, MediaSpool, MediaSpoolError, MediaUpload, OpenedMedia,
            ProviderEventStream, ProviderOutput, ProviderRequest, ProviderTransport,
            TransportError, TransportPhase, UpstreamSignal,
        },
        routing::{
            provider::{Provider, ProviderKind},
            selection::AttemptPlan,
            snapshot::{RuntimeGeneration, Snapshot},
        },
    };
    use crate::inference::{
        circuit::Breaker,
        failover::{
            BASE_RETRY_BACKOFF, Context, ExecutionFailure, ExecutionSuccess, MAX_RETRY_AFTER_DELAY,
            execute,
        },
        runtime::{Bundle, Manager},
    };

    struct UnavailableSpool;

    impl MediaSpool for UnavailableSpool {
        fn put(&self, _: MediaUpload) -> BoxFuture<'_, Result<MediaArtifact, MediaSpoolError>> {
            Box::pin(async { Err(MediaSpoolError::Unavailable) })
        }

        fn open<'a>(
            &'a self,
            _: &'a MediaHandle,
        ) -> BoxFuture<'a, Result<OpenedMedia, MediaSpoolError>> {
            Box::pin(async { Err(MediaSpoolError::Unavailable) })
        }

        fn remove<'a>(&'a self, _: &'a MediaHandle) -> BoxFuture<'a, Result<(), MediaSpoolError>> {
            Box::pin(async { Err(MediaSpoolError::Unavailable) })
        }
    }

    type Scripted = Result<Vec<Event>, TransportError>;

    /// Answers each call with the next scripted outcome; an exhausted script
    /// hangs before the first byte so the attempt timeout is what ends it.
    struct ScriptedTransport {
        script: Mutex<VecDeque<Scripted>>,
        calls: Arc<AtomicUsize>,
    }

    impl ProviderTransport for ScriptedTransport {
        fn execute(
            &self,
            _: ProviderRequest,
        ) -> BoxFuture<'_, Result<ProviderOutput, TransportError>> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            let next = self.script.lock().unwrap().pop_front();
            Box::pin(async move {
                match next {
                    Some(Ok(events)) => Ok(ProviderOutput::Events(Box::pin(stream::iter(
                        events.into_iter().map(Ok),
                    ))
                        as ProviderEventStream)),
                    Some(Err(error)) => Err(error),
                    None => Ok(ProviderOutput::Events(
                        Box::pin(stream::pending()) as ProviderEventStream
                    )),
                }
            })
        }
    }

    fn success_events() -> Vec<Event> {
        vec![
            Event::new(
                0,
                Kind::ResponseStart {
                    response_id: Some("response-upstream".into()),
                    provider_model: Some("upstream-model".into()),
                },
            ),
            Event::new(
                1,
                Kind::MessageStart {
                    output_index: 0,
                    role: MessageRole::Assistant,
                },
            ),
            Event::new(
                2,
                Kind::TextDelta {
                    output_index: 0,
                    text: "recovered".into(),
                },
            ),
            Event::new(
                3,
                Kind::Finish {
                    output_index: 0,
                    reason: FinishReason::Stop,
                },
            ),
            Event::new(4, Kind::Done),
        ]
    }

    fn rate_limited(retry_after: Duration) -> TransportError {
        let mut error = failure(TransportPhase::FirstByte, AttemptFailureClass::RateLimit);
        error.upstream = UpstreamSignal::from_status(429).with_retry_after(Some(retry_after));
        error
    }

    fn committed_failure() -> TransportError {
        let mut error = failure(TransportPhase::Body, AttemptFailureClass::UpstreamServer);
        error.response_committed = true;
        error
    }

    fn succeeded(
        outcome: Result<ExecutionSuccess, ExecutionFailure>,
        expectation: &str,
    ) -> ExecutionSuccess {
        match outcome {
            Ok(success) => success,
            Err(failure) => panic!("{expectation}: {}", failure.error.message()),
        }
    }

    fn failed(
        outcome: Result<ExecutionSuccess, ExecutionFailure>,
        expectation: &str,
    ) -> ExecutionFailure {
        match outcome {
            Ok(success) => panic!(
                "{expectation}: succeeded after {} attempts",
                success.attempts.len()
            ),
            Err(failure) => failure,
        }
    }

    struct Target {
        plan: AttemptPlan,
        calls: Arc<AtomicUsize>,
    }

    struct Fixture {
        runtime: Arc<Bundle>,
        targets: Vec<Target>,
        circuits: Breaker,
    }

    impl Fixture {
        fn new(scripts: Vec<Vec<Scripted>>) -> Self {
            let manager = Manager::empty();
            let mut providers = BTreeMap::new();
            let mut transports: BTreeMap<ProviderId, Arc<dyn ProviderTransport>> = BTreeMap::new();
            let mut targets = Vec::new();
            for script in scripts {
                let mut plan = plan(TargetId::new());
                plan.provider_id = ProviderId::new();
                let calls = Arc::new(AtomicUsize::new(0));
                providers.insert(
                    plan.provider_id,
                    Provider {
                        id: plan.provider_id,
                        name: "scripted".into(),
                        kind: ProviderKind::OpenAi,
                        enabled: true,
                        active_credential: None,
                        capabilities: Default::default(),
                    },
                );
                transports.insert(
                    plan.provider_id,
                    Arc::new(ScriptedTransport {
                        script: Mutex::new(script.into()),
                        calls: calls.clone(),
                    }),
                );
                targets.push(Target { plan, calls });
            }
            let snapshot = Snapshot {
                generation: RuntimeGeneration {
                    id: RuntimeGenerationId::new(),
                    ordinal: 1,
                    activated_at: Utc::now(),
                },
                providers,
                routes: Default::default(),
                api_keys: Default::default(),
            };
            manager.install(snapshot, transports).unwrap();
            Self {
                runtime: manager.pin(),
                targets,
                circuits: Breaker::default(),
            }
        }

        fn calls(&self, index: usize) -> usize {
            self.targets[index].calls.load(Ordering::SeqCst)
        }

        async fn run(
            &self,
            overall_timeout: Duration,
            max_attempts: u16,
        ) -> (Result<ExecutionSuccess, ExecutionFailure>, Duration) {
            let started = tokio::time::Instant::now();
            let outcome = execute(
                Context {
                    runtime: &self.runtime,
                    overall_timeout,
                    max_attempts: NonZeroU16::new(max_attempts).unwrap(),
                    media_spool: Arc::new(UnavailableSpool),
                    circuits: &self.circuits,
                    on_attempt_started: None,
                },
                self.targets
                    .iter()
                    .map(|target| target.plan.clone())
                    .collect(),
                RequestMetadata {
                    request_id: RequestId::new(),
                    operation: OperationKind::Generation,
                    surface: Surface::OpenAi,
                    mode: TransportMode::Streaming,
                },
                Operation::Generation(GenerationRequest {
                    route: RouteSlug::parse("scripted-route").unwrap(),
                    messages: vec![Message {
                        role: MessageRole::User,
                        content: vec![ContentPart::Text {
                            text: "metadata-free fixture".into(),
                        }],
                        name: None,
                        tool_call_id: None,
                        tool_calls: Vec::new(),
                    }],
                    parameters: GenerationParameters {
                        max_output_tokens: Some(32),
                        ..GenerationParameters::default()
                    },
                    tools: Vec::new(),
                    tool_choice: None,
                    response_format: None,
                    extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
                }),
            )
            .await;
            (outcome, started.elapsed())
        }
    }

    #[tokio::test(start_paused = true)]
    async fn a_retryable_failure_backs_off_then_succeeds_on_the_next_target() {
        let fixture = Fixture::new(vec![
            vec![Err(failure(
                TransportPhase::Connect,
                AttemptFailureClass::Connect,
            ))],
            vec![Ok(success_events())],
        ]);

        let (outcome, elapsed) = fixture.run(Duration::from_secs(30), 2).await;

        let success = succeeded(outcome, "second target recovers the request");
        assert_eq!(success.attempts.len(), 2);
        assert_eq!(fixture.calls(0), 1);
        assert_eq!(fixture.calls(1), 1);
        assert!(
            (BASE_RETRY_BACKOFF / 2..=BASE_RETRY_BACKOFF).contains(&elapsed),
            "{elapsed:?}"
        );
    }

    #[tokio::test(start_paused = true)]
    async fn a_committed_failure_never_reaches_the_next_target() {
        let fixture = Fixture::new(vec![
            vec![Err(committed_failure())],
            vec![Ok(success_events())],
        ]);

        let (outcome, elapsed) = fixture.run(Duration::from_secs(30), 2).await;

        let failure = failed(outcome, "a committed response cannot be retried");
        assert_eq!(failure.attempts.len(), 1);
        assert_eq!(fixture.calls(1), 0);
        assert_eq!(elapsed, Duration::ZERO);
    }

    #[tokio::test(start_paused = true)]
    async fn a_deadline_expiring_during_backoff_stops_instead_of_sleeping() {
        let fixture = Fixture::new(vec![
            vec![Err(failure(
                TransportPhase::Connect,
                AttemptFailureClass::Connect,
            ))],
            vec![Ok(success_events())],
        ]);

        let (outcome, elapsed) = fixture.run(BASE_RETRY_BACKOFF / 4, 2).await;

        let failure = failed(outcome, "no room for another attempt");
        assert_eq!(failure.attempts.len(), 1);
        assert_eq!(fixture.calls(1), 0);
        assert!(elapsed < BASE_RETRY_BACKOFF / 4, "{elapsed:?}");
    }

    #[tokio::test(start_paused = true)]
    async fn an_upstream_retry_after_is_capped_and_scoped_to_the_same_target() {
        let hint = Duration::from_secs(120);
        let sole = Fixture::new(vec![vec![Err(rate_limited(hint)), Ok(success_events())]]);
        let (outcome, elapsed) = sole.run(Duration::from_secs(600), 2).await;
        succeeded(outcome, "the same target recovers after the capped wait");
        assert_eq!(sole.calls(0), 2);
        assert_eq!(elapsed, MAX_RETRY_AFTER_DELAY);

        let pair = Fixture::new(vec![
            vec![Err(rate_limited(hint))],
            vec![Ok(success_events())],
        ]);
        let (outcome, elapsed) = pair.run(Duration::from_secs(600), 2).await;
        succeeded(
            outcome,
            "another target is not bound by the first one's hint",
        );
        assert_eq!(pair.calls(1), 1);
        assert!(elapsed <= BASE_RETRY_BACKOFF, "{elapsed:?}");
    }

    #[tokio::test(start_paused = true)]
    async fn max_attempts_of_one_runs_exactly_one_attempt() {
        let fixture = Fixture::new(vec![vec![
            Err(failure(
                TransportPhase::Connect,
                AttemptFailureClass::Connect,
            )),
            Ok(success_events()),
        ]]);

        let (outcome, _) = fixture.run(Duration::from_secs(30), 1).await;

        let failure = failed(outcome, "the operator allowed a single attempt");
        assert_eq!(failure.attempts.len(), 1);
        assert_eq!(fixture.calls(0), 1);
    }

    #[tokio::test(start_paused = true)]
    async fn a_first_byte_timeout_is_not_re_sent_to_the_same_target() {
        let fixture = Fixture::new(vec![Vec::new()]);

        let (outcome, _) = fixture.run(Duration::from_secs(30), 2).await;

        let failure = failed(outcome, "a hung provider fails the request");
        assert_eq!(failure.attempts.len(), 1);
        assert_eq!(failure.attempts[0].error_class.as_deref(), Some("timeout"));
        assert_eq!(fixture.calls(0), 1);
    }

    #[tokio::test(start_paused = true)]
    async fn a_circuit_open_next_target_is_skipped_without_sleeping() {
        let fixture = Fixture::new(vec![
            vec![Err(failure(
                TransportPhase::Connect,
                AttemptFailureClass::Connect,
            ))],
            vec![Ok(success_events())],
        ]);
        let second = fixture.targets[1].plan.routing_id;
        while fixture.circuits.is_selectable(second) {
            fixture
                .circuits
                .record_failure(second, AttemptFailureClass::Connect);
        }

        let (outcome, elapsed) = fixture.run(Duration::from_secs(30), 2).await;

        failed(outcome, "every remaining target is unavailable");
        assert_eq!(fixture.calls(1), 0);
        assert_eq!(elapsed, Duration::ZERO);
    }
}
