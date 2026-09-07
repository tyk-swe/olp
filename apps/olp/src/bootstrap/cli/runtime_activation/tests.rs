use std::{
    collections::BTreeMap,
    num::{NonZeroU16, NonZeroU32},
};

use chrono::Utc;
use olp_engine::domain::{
    ids::{DurationMs, RouteId, RouteSlug, RuntimeGenerationId, TargetId},
    ports::{AttemptFailureClass, BoxFuture, ProviderOutput, ProviderRequest, TransportError},
    routing::{
        provider::{Provider, ProviderKind},
        route::{Route, Target},
        snapshot::RuntimeGeneration,
    },
};
use sqlx::postgres::PgPoolOptions;
use tokio::sync::Barrier;

use super::*;

struct UnusedTransport;

impl ProviderTransport for UnusedTransport {
    fn execute<'a>(
        &'a self,
        _request: ProviderRequest,
    ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        Box::pin(async { panic!("activation must not execute provider requests") })
    }
}

fn activator() -> RuntimeActivator {
    RuntimeActivator {
        runtime: Arc::new(Manager::empty()),
        store: Store::from_pool(
            PgPoolOptions::new()
                .connect_lazy("postgres://localhost/activation_unit_test")
                .unwrap(),
        ),
        transports: TransportRegistry::default(),
        circuits: Breaker::default(),
        master_key: None,
        egress_policy: Arc::new(EgressPolicy::default()),
        response_limits: ResponseLimits::default(),
        activation_lock: Arc::new(Mutex::new(())),
        after_publication: None,
    }
}

fn snapshot(ordinal: u64, target_ids: &[TargetId]) -> Snapshot {
    let provider_id = ProviderId::new();
    let slug = RouteSlug::parse("activation-test").unwrap();
    Snapshot {
        generation: RuntimeGeneration {
            id: RuntimeGenerationId::new(),
            ordinal,
            activated_at: Utc::now(),
        },
        providers: BTreeMap::from([(
            provider_id,
            Provider {
                id: provider_id,
                revision_id: None,
                name: "activation-test".into(),
                kind: ProviderKind::OpenAi,
                enabled: true,
                active_credential: None,
                capabilities: Default::default(),
            },
        )]),
        routes: BTreeMap::from([(
            slug.clone(),
            Route {
                id: RouteId::new(),
                routing_id: None,
                slug,
                operations: Default::default(),
                overall_timeout: DurationMs::new(100),
                max_attempts: NonZeroU16::new(1).unwrap(),
                targets: target_ids
                    .iter()
                    .map(|target| Target {
                        id: TargetId::new(),
                        routing_id: Some(*target),
                        provider_id,
                        upstream_model: "test-model".into(),
                        priority: 0,
                        weight: NonZeroU32::new(1).unwrap(),
                        timeout: DurationMs::new(100),
                    })
                    .collect(),
            },
        )]),
        api_keys: Default::default(),
    }
}

async fn install(activator: &RuntimeActivator, snapshot: Snapshot) -> AppResult<bool> {
    let activation = activator.activation_lock.lock().await;
    let transports = snapshot
        .providers
        .keys()
        .map(|provider_id| {
            (
                *provider_id,
                Arc::new(UnusedTransport) as Arc<dyn ProviderTransport>,
            )
        })
        .collect();
    activator
        .install_candidate(&activation, snapshot, transports)
        .await
}

fn open_circuit(breaker: &Breaker, target: TargetId) {
    for _ in 0..5 {
        breaker.record_failure(target, AttemptFailureClass::Connect);
    }
    assert!(!breaker.is_selectable(target));
}

#[tokio::test]
async fn activation_waits_for_the_previous_poll_before_reading_storage() {
    let first = activator();
    let second = first.clone();
    first.store.pool().close().await;
    let activation = first.activation_lock.lock().await;
    let mut pending = Box::pin(second.activate());

    assert!(futures::poll!(&mut pending).is_pending());
    drop(activation);
    assert!(pending.await.is_err());
}

#[tokio::test]
async fn older_publication_finishes_pruning_before_a_newer_activation_can_publish() {
    let mut first = activator();
    let barrier = Arc::new(Barrier::new(2));
    first.after_publication = Some(barrier.clone());
    let mut second = first.clone();
    second.after_publication = None;
    let retained = TargetId::new();
    let removed = TargetId::new();
    let added = TargetId::new();
    let older_snapshot = snapshot(1, &[retained, removed]);
    let newer_snapshot = snapshot(2, &[retained, added]);
    let mut older = Box::pin(install(&first, older_snapshot.clone()));

    assert!(futures::poll!(&mut older).is_pending());
    let pinned = first.runtime.pin();
    assert_eq!(pinned.generation.ordinal, 1);
    open_circuit(&first.circuits, retained);
    open_circuit(&first.circuits, removed);
    let mut newer = Box::pin(install(&second, newer_snapshot));
    assert!(futures::poll!(&mut newer).is_pending());
    assert_eq!(first.runtime.active_generation_ordinal(), Some(1));

    barrier.wait().await;
    assert!(older.await.unwrap());
    assert!(!first.circuits.is_selectable(removed));
    assert!(newer.await.unwrap());
    open_circuit(&first.circuits, added);
    assert_eq!(first.runtime.active_generation_ordinal(), Some(2));
    assert!(!first.circuits.is_selectable(retained));
    assert!(first.circuits.is_selectable(removed));
    assert!(!install(&second, older_snapshot).await.unwrap());
    assert!(!first.circuits.is_selectable(added));
    assert!(!first.circuits.is_selectable(retained));
    assert_eq!(first.circuits.open_count(), 2);
    assert_eq!(first.runtime.active_generation_ordinal(), Some(2));
    assert_eq!(pinned.generation.ordinal, 1);
    assert!(
        pinned
            .routes
            .values()
            .next()
            .unwrap()
            .targets
            .iter()
            .any(|target| target.routing_id == Some(removed))
    );
}
