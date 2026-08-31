use std::{
    collections::BTreeSet,
    num::{NonZeroU32, NonZeroU64},
    sync::atomic::{AtomicBool, AtomicI64, AtomicUsize},
};

use crate::domain::{
    auth::{ApiKeyDigest, ApiKeyLimits, ApiKeyStatus},
    ids::{ApiKeyId, ApiKeyLookupId},
};
use serde_json::json;

use crate::inference::error::Kind as InferenceErrorKind;

use super::*;

#[derive(Debug, Eq, PartialEq)]
struct CapturedRequest {
    lookup_id: String,
    requests_per_minute: Option<i64>,
    tokens_per_minute: Option<i64>,
    max_concurrency: Option<i64>,
    requested_tokens: i64,
    lease_ttl: Duration,
}

#[derive(Default)]
struct BackendCalls {
    reserves: AtomicUsize,
    reconciles: AtomicUsize,
    releases: AtomicUsize,
    actual_tokens: AtomicI64,
    fail_reconcile_once: AtomicBool,
    fail_release_once: AtomicBool,
    requests: Mutex<Vec<CapturedRequest>>,
}

#[derive(Clone, Copy)]
enum BackendBehavior {
    Success,
    Exceeded(LimitDimension),
    Failure,
}

#[derive(Clone)]
struct FakeBackend {
    calls: Arc<BackendCalls>,
    behavior: BackendBehavior,
}

struct FakeLease {
    calls: Arc<BackendCalls>,
}

impl LimitLease for FakeLease {
    fn reconcile(&self, actual_tokens: i64) -> BoxFuture<'_, Result<(), LimitError>> {
        self.calls.reconciles.fetch_add(1, Ordering::Relaxed);
        self.calls
            .actual_tokens
            .store(actual_tokens, Ordering::Relaxed);
        let fail = self
            .calls
            .fail_reconcile_once
            .swap(false, Ordering::Relaxed);
        Box::pin(async move {
            if fail {
                Err(LimitError::UnexpectedResponse)
            } else {
                Ok(())
            }
        })
    }

    fn release(&self) -> BoxFuture<'_, Result<(), LimitError>> {
        self.calls.releases.fetch_add(1, Ordering::Relaxed);
        let fail = self.calls.fail_release_once.swap(false, Ordering::Relaxed);
        Box::pin(async move {
            if fail {
                Err(LimitError::UnexpectedResponse)
            } else {
                Ok(())
            }
        })
    }
}

impl LimitBackend for FakeBackend {
    fn reserve<'a>(
        &'a self,
        request: LimitRequest<'a>,
    ) -> BoxFuture<'a, Result<Arc<dyn LimitLease>, LimitError>> {
        let calls = Arc::clone(&self.calls);
        let behavior = self.behavior;
        Box::pin(async move {
            request.validate()?;
            calls.reserves.fetch_add(1, Ordering::Relaxed);
            calls.requests.lock().unwrap().push(CapturedRequest {
                lookup_id: request.lookup_id.to_owned(),
                requests_per_minute: request.requests_per_minute,
                tokens_per_minute: request.tokens_per_minute,
                max_concurrency: request.max_concurrency,
                requested_tokens: request.requested_tokens,
                lease_ttl: request.lease_ttl,
            });
            match behavior {
                BackendBehavior::Success => {
                    Ok(Arc::new(FakeLease { calls }) as Arc<dyn LimitLease>)
                }
                BackendBehavior::Exceeded(dimension) => Err(LimitError::Exceeded {
                    dimension,
                    retry_after: Duration::from_secs(2),
                }),
                BackendBehavior::Failure => Err(LimitError::MalformedState),
            }
        })
    }

    fn ping(&self) -> BoxFuture<'_, Result<(), LimitError>> {
        Box::pin(async { Ok(()) })
    }
}

fn backend(calls: &Arc<BackendCalls>, behavior: BackendBehavior) -> FakeBackend {
    FakeBackend {
        calls: Arc::clone(calls),
        behavior,
    }
}

fn api_key(limits: ApiKeyLimits) -> ApiKey {
    ApiKey {
        id: ApiKeyId::new(),
        lookup_id: ApiKeyLookupId::parse("lookup_01").unwrap(),
        digest: ApiKeyDigest::new([7; 32]),
        status: ApiKeyStatus::Active,
        expires_at: None,
        scopes: BTreeSet::new(),
        allowed_routes: BTreeSet::new(),
        limits,
    }
}

fn text_count(text: &str) -> Operation {
    operation(
        "token_count",
        json!({"route": "default", "input": [{"type": "text", "text": text}]}),
    )
}

fn operation(kind: &str, request: serde_json::Value) -> Operation {
    serde_json::from_value(json!({"operation": kind, "request": request})).unwrap()
}

async fn reserve(
    limiter: &ReloadableLimiter,
    key: &ApiKey,
    operation: &Operation,
    pre_reserved: Option<i64>,
) -> Result<Option<Reservation>, InferenceError> {
    super::reserve(
        limiter,
        key,
        operation,
        key.lookup_id.as_str(),
        Duration::from_secs(30),
        pre_reserved,
    )
    .await
}

#[test]
fn limit_requests_validate_every_backend_invariant() {
    let valid = LimitRequest {
        lookup_id: "lookup_01",
        requests_per_minute: Some(10),
        tokens_per_minute: Some(100),
        max_concurrency: Some(2),
        requested_tokens: 20,
        lease_ttl: Duration::from_secs(30),
    };
    assert!(valid.has_hard_limits());
    assert!(valid.validate().is_ok());

    type Mutator = for<'a> fn(&mut LimitRequest<'a>);
    let cases: [(&str, Mutator); 7] = [
        ("API key lookup ID", |r| r.lookup_id = "short"),
        ("requests_per_minute", |r| r.requests_per_minute = Some(0)),
        ("tokens_per_minute", |r| r.tokens_per_minute = Some(-1)),
        ("max_concurrency", |r| r.max_concurrency = Some(0)),
        ("non-negative", |r| r.requested_tokens = -1),
        ("positive when", |r| r.requested_tokens = 0),
        ("lease TTL", |r| r.lease_ttl = Duration::ZERO),
    ];
    for (expected, mutate) in cases {
        let mut request = valid.clone();
        mutate(&mut request);
        assert!(matches!(
            request.validate(),
            Err(LimitError::InvalidRequest(message)) if message.contains(expected)
        ));
    }

    let unlimited = LimitRequest {
        lookup_id: "lookup_01",
        requests_per_minute: None,
        tokens_per_minute: None,
        max_concurrency: None,
        requested_tokens: 0,
        lease_ttl: Duration::from_secs(1),
    };
    assert!(!unlimited.has_hard_limits());
    assert!(unlimited.validate().is_ok());
}

#[tokio::test]
async fn full_limit_reservation_forwards_all_dimensions_and_reconciles_usage() {
    let calls = Arc::new(BackendCalls::default());
    let limiter = ReloadableLimiter::default();
    limiter.install(backend(&calls, BackendBehavior::Success));
    let key = api_key(ApiKeyLimits {
        requests_per_minute: NonZeroU32::new(10),
        tokens_per_minute: NonZeroU64::new(100),
        concurrency: NonZeroU32::new(2),
    });

    let reservation = super::reserve(
        &limiter,
        &key,
        &text_count("12345678"),
        key.lookup_id.as_str(),
        Duration::from_secs(45),
        None,
    )
    .await
    .unwrap();
    assert_eq!(
        calls.requests.lock().unwrap().as_slice(),
        [CapturedRequest {
            lookup_id: "lookup_01".into(),
            requests_per_minute: Some(10),
            tokens_per_minute: Some(100),
            max_concurrency: Some(2),
            requested_tokens: 2,
            lease_ttl: Duration::from_secs(45),
        }]
    );

    release(reservation, Some(1)).await;
    assert_eq!(calls.reconciles.load(Ordering::Relaxed), 1);
    assert_eq!(calls.actual_tokens.load(Ordering::Relaxed), 1);
    assert_eq!(calls.releases.load(Ordering::Relaxed), 1);
}

#[tokio::test]
async fn http_token_reservation_charges_only_a_positive_estimate_delta() {
    let limiter = ReloadableLimiter::default();
    let no_token_limit = api_key(ApiKeyLimits::default());
    assert!(
        reserve(&limiter, &no_token_limit, &text_count("12345678"), Some(1))
            .await
            .unwrap()
            .is_none()
    );

    let token_limited = api_key(ApiKeyLimits {
        tokens_per_minute: NonZeroU64::new(100),
        ..ApiKeyLimits::default()
    });
    assert!(
        reserve(&limiter, &token_limited, &text_count("12345678"), Some(2))
            .await
            .unwrap()
            .is_none()
    );

    let calls = Arc::new(BackendCalls::default());
    limiter.install(backend(&calls, BackendBehavior::Success));
    let reservation = reserve(
        &limiter,
        &token_limited,
        &text_count("123456789012"),
        Some(1),
    )
    .await
    .unwrap();
    {
        let requests = calls.requests.lock().unwrap();
        assert_eq!(requests[0].requests_per_minute, None);
        assert_eq!(requests[0].tokens_per_minute, Some(100));
        assert_eq!(requests[0].max_concurrency, None);
        assert_eq!(requests[0].requested_tokens, 2);
    }
    release(reservation, None).await;
}

#[tokio::test]
async fn http_delta_reservation_rejects_a_request_larger_than_the_whole_tpm_budget() {
    let calls = Arc::new(BackendCalls::default());
    let limiter = ReloadableLimiter::default();
    limiter.install(backend(&calls, BackendBehavior::Success));
    let key = api_key(ApiKeyLimits {
        tokens_per_minute: NonZeroU64::new(2),
        ..ApiKeyLimits::default()
    });

    let error = reserve(&limiter, &key, &text_count("123456789012"), Some(1))
        .await
        .err()
        .expect("the complete request cannot fit within the token budget");

    assert_eq!(error.code(), "request_exceeds_token_limit");
    assert_eq!(error.kind(), InferenceErrorKind::InvalidRequest);
    assert_eq!(calls.reserves.load(Ordering::Relaxed), 0);
}

#[tokio::test]
async fn a_request_larger_than_the_whole_tpm_budget_is_a_client_error_not_a_rate_limit() {
    // A 2000 TPM key against the 4096-token default output estimate: the Lua
    // script rejects `requested > limit` outright, so reporting it as a 429
    // with a Retry-After pointed the caller at a minute that never arrives.
    let key = api_key(ApiKeyLimits {
        requests_per_minute: None,
        tokens_per_minute: NonZeroU64::new(2_000),
        concurrency: None,
    });
    let limiter = ReloadableLimiter::default();
    let calls = Arc::new(BackendCalls::default());
    limiter.install(backend(&calls, BackendBehavior::Success));

    let generation = operation(
        "generation",
        json!({
            "route": "default",
            "messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}],
            "parameters": {"stream": false},
            "tools": [],
            "tool_choice": null,
            "response_format": null
        }),
    );
    let error = reserve(&limiter, &key, &generation, None)
        .await
        .err()
        .expect("an impossible request cannot be admitted");
    assert_eq!(error.code(), "request_exceeds_token_limit");
    assert_eq!(error.kind(), InferenceErrorKind::InvalidRequest);
    assert_eq!(
        error.retry_after(),
        None,
        "no amount of waiting makes this request fit"
    );
    assert_eq!(
        calls.reserves.load(std::sync::atomic::Ordering::Relaxed),
        0,
        "the limiter is never consulted for a request that cannot fit"
    );

    // A request that does fit still reaches the limiter.
    assert!(
        reserve(&limiter, &key, &text_count("12345678"), None)
            .await
            .is_ok()
    );
}

#[tokio::test]
async fn both_reservation_paths_map_exceeded_and_backend_failures_fail_closed() {
    let key = api_key(ApiKeyLimits {
        requests_per_minute: NonZeroU32::new(10),
        tokens_per_minute: NonZeroU64::new(100),
        concurrency: None,
    });
    for (behavior, expected_code) in [
        (
            BackendBehavior::Exceeded(LimitDimension::Tokens),
            "rate_limit_exceeded",
        ),
        (BackendBehavior::Failure, "distributed_limits_unavailable"),
    ] {
        for pre_reserved in [None, Some(1)] {
            let limiter = ReloadableLimiter::default();
            let calls = Arc::new(BackendCalls::default());
            limiter.install(backend(&calls, behavior));
            let error = reserve(&limiter, &key, &text_count("123456789012"), pre_reserved)
                .await
                .err()
                .unwrap();
            assert_eq!(error.code(), expected_code);
            if matches!(behavior, BackendBehavior::Exceeded(_)) {
                assert_eq!(error.retry_after(), Some(Duration::from_secs(2)));
            }
        }
    }

    let unavailable = ReloadableLimiter::default();
    assert_eq!(
        reserve(&unavailable, &key, &text_count("1234"), None)
            .await
            .err()
            .unwrap()
            .code(),
        "distributed_limits_unavailable"
    );
}

#[tokio::test]
async fn shared_reservation_retries_cleanup_and_runs_each_action_once() {
    let calls = Arc::new(BackendCalls::default());
    calls.fail_reconcile_once.store(true, Ordering::Relaxed);
    calls.fail_release_once.store(true, Ordering::Relaxed);
    let lease: Arc<dyn LimitLease> = Arc::new(FakeLease {
        calls: Arc::clone(&calls),
    });
    let reservation = Reservation::distributed(lease);
    let clone = reservation.clone();

    reservation.reconcile(17).await;
    clone.reconcile(99).await;
    clone.release().await;
    reservation.release().await;

    assert_eq!(calls.reconciles.load(Ordering::Relaxed), 2);
    assert_eq!(calls.actual_tokens.load(Ordering::Relaxed), 17);
    assert_eq!(calls.releases.load(Ordering::Relaxed), 2);
}

#[tokio::test]
async fn dropping_a_test_reservation_schedules_its_release() {
    let (released, observed) = tokio::sync::oneshot::channel();
    let reservation = Reservation::for_test(async move {
        let _ = released.send(());
    });
    drop(reservation);
    tokio::time::timeout(Duration::from_secs(1), observed)
        .await
        .expect("drop release should be scheduled")
        .expect("drop release should run");
}

#[test]
fn token_estimation_covers_generation_metadata_and_every_media_weight() {
    let generation = operation(
        "generation",
        json!({
            "route": "default",
            "messages": [{
                "role": "assistant",
                "content": [{"type": "text", "text": "1234"}],
                "name": "name",
                "tool_call_id": "call",
                "tool_calls": [{"id": "ignored", "name": "lookup", "arguments": "{}"}]
            }],
            "parameters": {"max_output_tokens": 10, "candidate_count": 2, "stream": false},
            "tools": [{"name": "tool", "description": "desc", "input_schema": null}],
            "tool_choice": null,
            "response_format": null
        }),
    );
    assert_eq!(estimate_tokens(&generation), 29);

    let Operation::TokenCount(media) = operation(
        "token_count",
        json!({
            "route": "default",
            "input": [
                {"type": "text", "text": "12345678"},
                {"type": "refusal", "text": "x"},
                {"type": "image", "source": {"kind": "uri", "value": "https://example.test/image"}, "detail": null},
                {"type": "input_audio", "media": "audio", "format": "wav"},
                {"type": "input_file", "media": "file", "mime_type": "text/plain", "filename": "input.txt"}
            ]
        }),
    ) else {
        unreachable!()
    };
    assert_eq!(estimated_content_tokens(&media.input), 5_003);
}

#[test]
fn token_estimation_is_defined_for_each_non_generation_operation_family() {
    let cases = [
        (
            "embeddings",
            r#"{"operation":"embeddings","request":{"route":"default","input":["12345",[1,2,3]],"dimensions":null}}"#,
            5,
        ),
        (
            "image generation",
            r#"{"operation":"images","request":{"kind":"generation","request":{"route":"default","prompt":"12345","count":null,"size":null,"stream":false}}}"#,
            2,
        ),
        (
            "image edit",
            r#"{"operation":"images","request":{"kind":"edit","request":{"route":"default","images":["a","b"],"mask":"c","prompt":"1234","stream":false}}}"#,
            3_001,
        ),
        (
            "image variation",
            r#"{"operation":"images","request":{"kind":"variation","request":{"route":"default","image":"media","count":null,"size":null}}}"#,
            1_000,
        ),
        (
            "speech",
            r#"{"operation":"speech","request":{"route":"default","input":"12345","voice":"voice","format":null,"stream":false}}"#,
            2,
        ),
        (
            "transcription",
            r#"{"operation":"transcription","request":{"route":"default","audio":"media","language":null,"prompt":"12345","stream":false}}"#,
            1_502,
        ),
        (
            "video create",
            r#"{"operation":"video","request":{"kind":"create","request":{"route":"default","prompt":"1234","input":"media"}}}"#,
            2_001,
        ),
        (
            "moderation",
            r#"{"operation":"moderation","request":{"route":"default","input":[{"type":"text","text":"12345678"}]}}"#,
            2,
        ),
        (
            "video metadata",
            r#"{"operation":"video","request":{"kind":"list","request":{"route":null,"cursor":null,"limit":null}}}"#,
            1,
        ),
        (
            "model metadata",
            r#"{"operation":"models","request":{"kind":"list"}}"#,
            1,
        ),
    ];
    for (name, value, expected) in cases {
        let operation: Operation = serde_json::from_str(value).unwrap();
        assert_eq!(estimate_tokens(&operation), expected, "{name}");
    }

    let no_prompt = operation(
        "transcription",
        json!({"route": "default", "audio": "media", "language": null, "prompt": null, "stream": false}),
    );
    assert_eq!(estimate_tokens(&no_prompt), 1_500);
}

#[tokio::test]
async fn issued_lease_survives_backend_replacement_and_clear() {
    let first_calls = Arc::new(BackendCalls::default());
    let second_calls = Arc::new(BackendCalls::default());
    let limiter = ReloadableLimiter::default();
    limiter.install(backend(&first_calls, BackendBehavior::Success));

    let lease = limiter
        .current()
        .unwrap()
        .reserve(LimitRequest {
            lookup_id: "lookup_01",
            requests_per_minute: Some(10),
            tokens_per_minute: Some(100),
            max_concurrency: Some(2),
            requested_tokens: 20,
            lease_ttl: Duration::from_secs(30),
        })
        .await
        .unwrap();

    limiter.install(backend(&second_calls, BackendBehavior::Success));
    limiter.clear();
    let reservation = Reservation::distributed(lease);
    reservation.reconcile(13).await;
    reservation.release().await;

    assert_eq!(first_calls.reserves.load(Ordering::Relaxed), 1);
    assert_eq!(first_calls.reconciles.load(Ordering::Relaxed), 1);
    assert_eq!(first_calls.releases.load(Ordering::Relaxed), 1);
    assert_eq!(first_calls.actual_tokens.load(Ordering::Relaxed), 13);
    assert_eq!(second_calls.reserves.load(Ordering::Relaxed), 0);
    assert_eq!(second_calls.reconciles.load(Ordering::Relaxed), 0);
    assert_eq!(second_calls.releases.load(Ordering::Relaxed), 0);
    assert!(limiter.current().is_none());
}

fn hard_limited_key() -> ApiKey {
    api_key(ApiKeyLimits {
        requests_per_minute: NonZeroU32::new(10),
        tokens_per_minute: NonZeroU64::new(100),
        concurrency: None,
    })
}

#[tokio::test]
async fn fail_open_policy_admits_when_backend_missing() {
    let key = hard_limited_key();
    let limiter = ReloadableLimiter::default();
    limiter.mark_configured();
    limiter.set_outage_policy(LimitOutagePolicy::FailOpen);
    assert_eq!(limiter.outage_policy(), LimitOutagePolicy::FailOpen);
    for pre_reserved in [None, Some(1)] {
        assert!(
            reserve(&limiter, &key, &text_count("123456789012"), pre_reserved)
                .await
                .unwrap()
                .is_none()
        );
    }
    let calls = Arc::new(BackendCalls::default());
    limiter.install(backend(&calls, BackendBehavior::Failure));
    assert!(
        reserve(&limiter, &key, &text_count("1234"), None)
            .await
            .unwrap()
            .is_none()
    );
    assert_eq!(limiter.fail_open_total(), 3);
}

#[tokio::test]
async fn fail_open_policy_requires_configured_valkey() {
    let key = hard_limited_key();
    let limiter = ReloadableLimiter::default();
    limiter.set_outage_policy(LimitOutagePolicy::FailOpen);
    assert_eq!(
        reserve(&limiter, &key, &text_count("1234"), None)
            .await
            .err()
            .unwrap()
            .code(),
        "distributed_limits_unavailable"
    );
    assert_eq!(limiter.fail_open_total(), 0);
}

#[tokio::test]
async fn exceeded_limits_still_reject_under_fail_open() {
    let key = hard_limited_key();
    let limiter = ReloadableLimiter::default();
    limiter.mark_configured();
    limiter.set_outage_policy(LimitOutagePolicy::FailOpen);
    let calls = Arc::new(BackendCalls::default());
    limiter.install(backend(
        &calls,
        BackendBehavior::Exceeded(LimitDimension::Tokens),
    ));
    let error = reserve(&limiter, &key, &text_count("123456789012"), None)
        .await
        .err()
        .unwrap();
    assert_eq!(error.code(), "rate_limit_exceeded");
    assert_eq!(limiter.fail_open_total(), 0);
    limiter.set_outage_policy(LimitOutagePolicy::FailClosed);
    limiter.clear();
    assert_eq!(
        reserve(&limiter, &key, &text_count("1234"), None)
            .await
            .err()
            .unwrap()
            .code(),
        "distributed_limits_unavailable"
    );
}
