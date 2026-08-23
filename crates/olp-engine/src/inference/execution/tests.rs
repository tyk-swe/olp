use std::{
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
            requests::MediaHandle,
            results::MediaArtifact,
        },
        ports::{
            BoxFuture, MediaSpool, MediaSpoolError, MediaUpload, OpenedMedia, ProviderEventStream,
        },
    },
    inference::{
        accounting::RequestAccountingInput,
        circuit::Breaker,
        limits::{LimitError, LimitLease, ReloadableLimiter},
        runtime::Manager,
    },
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
        accounting: Some(accounting),
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
