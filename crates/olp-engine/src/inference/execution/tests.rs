use std::{
    collections::{BTreeMap, BTreeSet},
    num::{NonZeroU16, NonZeroU32},
    sync::{
        Arc,
        atomic::{AtomicI64, AtomicUsize, Ordering},
    },
    time::Duration,
};

use futures::stream;

use super::*;
use crate::{
    domain::{
        canonical::{
            events::{Kind, Usage},
            identity::TransportMode,
            requests::{MediaHandle, Operation, VideoOperation},
            results::{CanonicalResult, MediaArtifact, VideoJobResult, VideoStatus},
        },
        ids::{DurationMs, ProviderId, RouteId, RuntimeGenerationId, TargetId},
        ports::{
            BoxFuture, MediaSpool, MediaSpoolError, MediaUpload, OpenedMedia, ProviderEventStream,
            ProviderOutput, ProviderRequest, ProviderTransport, TransportError,
        },
        routing::{
            provider::{Capability, Provider, ProviderKind},
            route::{Route, Target},
            snapshot::{RuntimeGeneration, Snapshot},
        },
    },
    inference::{
        accounting::RequestAccountingInput,
        circuit::Breaker,
        limits::{LimitError, LimitLease, ReloadableLimiter},
        runtime::Manager,
    },
};

struct ReconciliationTransport(Arc<AtomicUsize>);

impl ProviderTransport for ReconciliationTransport {
    fn execute(&self, _: ProviderRequest) -> BoxFuture<'_, Result<ProviderOutput, TransportError>> {
        self.0.fetch_add(1, Ordering::SeqCst);
        Box::pin(async {
            Ok(ProviderOutput::Result(Box::new(CanonicalResult::VideoJob(
                VideoJobResult {
                    id: "upstream-job".into(),
                    model: Some("video-model".into()),
                    status: VideoStatus::InProgress,
                    progress_percent: Some(10.0),
                    created_at: None,
                    completed_at: None,
                    expires_at: None,
                    prompt: None,
                    seconds: None,
                    size: None,
                    error: None,
                    extensions: Default::default(),
                },
            ))))
        })
    }
}

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

#[derive(Default)]
struct CleanupEffects {
    gate: tokio::sync::Notify,
    started: tokio::sync::Notify,
    released: tokio::sync::Notify,
    reconciles: AtomicUsize,
    releases: AtomicUsize,
    actual_tokens: AtomicI64,
}

struct GatedLease(Arc<CleanupEffects>);

impl LimitLease for GatedLease {
    fn reconcile(&self, actual_tokens: i64) -> BoxFuture<'_, Result<(), LimitError>> {
        self.0.reconciles.fetch_add(1, Ordering::Relaxed);
        self.0.actual_tokens.store(actual_tokens, Ordering::Relaxed);
        Box::pin(async move {
            self.0.started.notify_one();
            self.0.gate.notified().await;
            Ok(())
        })
    }

    fn release(&self) -> BoxFuture<'_, Result<(), LimitError>> {
        self.0.releases.fetch_add(1, Ordering::Relaxed);
        self.0.released.notify_one();
        Box::pin(async { Ok(()) })
    }
}

fn routed_events(success: bool, effects: Arc<CleanupEffects>) -> RoutedEvents {
    let service = Service::new(
        Arc::new(Manager::empty()),
        ReloadableLimiter::default(),
        None,
        Breaker::default(),
        Arc::new(UnavailableSpool),
    );
    let request_id = uuid::Uuid::now_v7();
    let route_slug = RouteSlug::parse("test").unwrap();
    let accounting = RequestAccountingGuard::new(
        service,
        RequestAccountingInput {
            generation_id: uuid::Uuid::now_v7(),
            api_key_id: uuid::Uuid::now_v7(),
            request_id,
            route_slug: route_slug.clone(),
            request_started_at: Utc::now(),
            request_started: tokio::time::Instant::now(),
            surface: Surface::OpenAi,
            operation: OperationKind::Generation,
            trace: None,
        },
        None,
        Some(Reservation::distributed(Arc::new(GatedLease(Arc::clone(
            &effects,
        ))))),
        Some(100),
    );
    let events: ProviderEventStream = if success {
        Box::pin(stream::iter([Ok(Event::new(1, Kind::Done))]))
    } else {
        Box::pin(stream::empty())
    };
    RoutedEvents {
        first: Event::new(
            0,
            Kind::Usage {
                usage: Usage {
                    input_tokens: 7,
                    output_tokens: 3,
                    total_tokens: 10,
                    cached_input_tokens: None,
                    reasoning_tokens: None,
                },
            },
        ),
        events,
        deadline: tokio::time::Instant::now() + Duration::from_secs(1),
        request_id,
        route_slug,
        accounting,
        max_collected_bytes: crate::inference::events::MAX_COLLECTED_CANONICAL_EVENT_BYTES,
    }
}

#[tokio::test]
async fn unary_collection_detaches_success_and_post_usage_failure_cleanup() {
    for success in [true, false] {
        let effects = Arc::new(CleanupEffects::default());
        let collected = tokio::spawn(routed_events(success, Arc::clone(&effects)).collect());

        tokio::time::timeout(Duration::from_secs(1), effects.started.notified())
            .await
            .expect("reconciliation must start");
        let result = tokio::time::timeout(Duration::from_secs(1), collected)
            .await
            .expect("collection must not wait for reconciliation")
            .expect("collection task must not panic");
        assert_eq!(result.is_ok(), success);
        assert_eq!(effects.reconciles.load(Ordering::Relaxed), 1);
        assert_eq!(effects.releases.load(Ordering::Relaxed), 0);
        assert_eq!(effects.actual_tokens.load(Ordering::Relaxed), 10);

        effects.gate.notify_one();
        tokio::time::timeout(Duration::from_secs(1), effects.released.notified())
            .await
            .expect("detached cleanup must release the reservation");
        assert_eq!(effects.reconciles.load(Ordering::Relaxed), 1);
        assert_eq!(effects.releases.load(Ordering::Relaxed), 1);
    }
}

#[tokio::test]
async fn reconciliation_uses_the_supplied_historical_bundle() {
    let manager = Arc::new(Manager::empty());
    let service = Service::new(
        Arc::clone(&manager),
        ReloadableLimiter::default(),
        None,
        Breaker::default(),
        Arc::new(UnavailableSpool),
    );
    let route_slug = RouteSlug::parse("historical-video").unwrap();
    let provider_id = ProviderId::new();
    let generation_id = RuntimeGenerationId::new();
    let target = Target {
        id: TargetId::new(),
        routing_id: None,
        provider_id,
        upstream_model: "video-model".into(),
        priority: 0,
        weight: NonZeroU32::new(1).unwrap(),
        timeout: DurationMs::new(1_000),
    };
    let route = Route {
        id: RouteId::new(),
        routing_id: None,
        slug: route_slug.clone(),
        operations: BTreeSet::from([OperationKind::VideoGet]),
        overall_timeout: DurationMs::new(2_000),
        max_attempts: NonZeroU16::new(1).unwrap(),
        targets: vec![target],
    };
    let snapshot = Snapshot {
        generation: RuntimeGeneration {
            id: generation_id,
            ordinal: 1,
            activated_at: Utc::now(),
        },
        providers: BTreeMap::from([(
            provider_id,
            Provider {
                id: provider_id,
                revision_id: None,
                name: "historical-provider".into(),
                kind: ProviderKind::OpenAi,
                enabled: true,
                active_credential: None,
                capabilities: BTreeSet::from([Capability::new(
                    "video-model",
                    OperationKind::VideoGet,
                    Surface::OpenAi,
                    TransportMode::Unary,
                )]),
            },
        )]),
        routes: BTreeMap::from([(route_slug.clone(), route)]),
        api_keys: BTreeMap::new(),
    };
    let calls = Arc::new(AtomicUsize::new(0));
    let transport: Arc<dyn ProviderTransport> =
        Arc::new(ReconciliationTransport(Arc::clone(&calls)));
    let historical = Manager::reconciliation_bundle(snapshot, provider_id, transport).unwrap();
    let mut operation = crate::protocols::openai::video::decode_video_get("upstream-job");
    let Operation::Video(VideoOperation::Get(request)) = &mut operation else {
        unreachable!()
    };
    request.route = Some(route_slug);

    let result = service
        .execute_reconciliation_result(
            historical,
            uuid::Uuid::now_v7(),
            operation,
            Surface::OpenAi,
            RequiredTarget {
                provider_id: provider_id.as_uuid(),
                upstream_model: "video-model".into(),
            },
        )
        .await
        .unwrap();

    assert!(matches!(*result, CanonicalResult::VideoJob(_)));
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    assert!(manager.pin().routes.is_empty());
}
