use std::{
    collections::BTreeSet,
    num::{NonZeroU16, NonZeroU32, NonZeroU64},
};

use crate::domain::{
    auth::{
        ApiKeyDigest, ApiKeyLimits, ApiKeyScope, ApiKeyStatus, GatewayCapability, authorize_api_key,
    },
    ids::{ApiKeyId, DurationMs, RouteId, RouteSlug, TargetId},
    ports::{BoxFuture, ProviderOutput, ProviderRequest, TransportError},
    routing::{
        provider::{Provider, ProviderKind},
        route::{Route, Target},
    },
};
use chrono::Duration;
use rust_decimal::Decimal;

use super::*;

struct PinnedTransport;

impl ProviderTransport for PinnedTransport {
    fn execute<'a>(
        &'a self,
        _request: ProviderRequest,
    ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        Box::pin(async { panic!("authority refresh must not execute a transport") })
    }
}

fn historical_key() -> ApiKey {
    ApiKey {
        id: ApiKeyId::new(),
        lookup_id: ApiKeyLookupId::parse("authority_refresh").unwrap(),
        digest: ApiKeyDigest::new([1; 32]),
        status: ApiKeyStatus::Active,
        expires_at: None,
        scopes: BTreeSet::from([ApiKeyScope::Inference]),
        allowed_routes: BTreeSet::new(),
        limits: ApiKeyLimits::default(),
    }
}

fn installed_manager(key: ApiKey) -> Manager {
    let manager = Manager::empty();
    let provider_id = ProviderId::new();
    let slug = RouteSlug::parse("retained-route").unwrap();
    let mut snapshot = manager.pin().snapshot.clone();
    snapshot.generation.ordinal = 7;
    snapshot.providers.insert(
        provider_id,
        Provider {
            id: provider_id,
            revision_id: None,
            name: "retained-provider".into(),
            kind: ProviderKind::OpenAi,
            enabled: true,
            active_credential: None,
            capabilities: Default::default(),
        },
    );
    snapshot.routes.insert(
        slug.clone(),
        Route {
            id: RouteId::new(),
            routing_id: None,
            slug,
            operations: Default::default(),
            overall_timeout: DurationMs::new(100),
            max_attempts: NonZeroU16::new(1).unwrap(),
            targets: vec![Target {
                id: TargetId::new(),
                routing_id: None,
                provider_id,
                upstream_model: "retained-model".into(),
                priority: 0,
                weight: NonZeroU32::new(1).unwrap(),
                timeout: DurationMs::new(100),
            }],
        },
    );
    snapshot.api_keys.insert(key.lookup_id.clone(), key);
    manager
        .install(
            snapshot,
            BTreeMap::from([(
                provider_id,
                Arc::new(PinnedTransport) as Arc<dyn ProviderTransport>,
            )]),
        )
        .unwrap();
    manager
}

#[test]
fn authority_refresh_preserves_pinned_routing_credentials_and_policy() {
    let historical = historical_key();
    let manager = installed_manager(historical.clone());
    let pinned = manager.pin();
    let route = RouteSlug::parse("retained-route").unwrap();
    let mut current = historical.clone();
    current.digest = ApiKeyDigest::new([2; 32]);
    current.expires_at = Some(Utc::now() + Duration::hours(1));
    current.scopes = BTreeSet::from([ApiKeyScope::ModelsRead]);
    current.allowed_routes = BTreeSet::from([route.clone()]);
    current.limits = ApiKeyLimits {
        requests_per_minute: NonZeroU32::new(7),
        tokens_per_minute: NonZeroU64::new(80),
        concurrency: NonZeroU32::new(2),
        daily_cost_limit: Some(Decimal::new(123, 2)),
        monthly_cost_limit: Some(Decimal::new(456, 2)),
    };
    manager
        .refresh_api_keys(BTreeMap::from([(
            current.lookup_id.clone(),
            current.clone(),
        )]))
        .unwrap();

    let refreshed = manager.pin();
    let key = &refreshed.api_keys[&current.lookup_id];
    assert_eq!(key.id, current.id);
    assert_eq!(key.digest, current.digest);
    assert_eq!(key.expires_at, current.expires_at);
    assert_eq!(key.scopes, current.scopes);
    assert_eq!(key.allowed_routes, current.allowed_routes);
    assert_eq!(key.limits, current.limits);
    assert!(
        authorize_api_key(
            key,
            Some(&route),
            Some(GatewayCapability::ModelsRead),
            GatewayCapability::ModelsRead,
            Utc::now()
        )
        .is_ok()
    );
    assert!(
        authorize_api_key(
            key,
            Some(&route),
            Some(GatewayCapability::Inference),
            GatewayCapability::Inference,
            Utc::now()
        )
        .is_err()
    );
    assert!(
        authorize_api_key(
            key,
            Some(&RouteSlug::parse("other-route").unwrap()),
            Some(GatewayCapability::ModelsRead),
            GatewayCapability::ModelsRead,
            Utc::now()
        )
        .is_err()
    );
    assert_eq!(refreshed.generation.id, pinned.generation.id);
    assert_eq!(
        refreshed.generation.activated_at,
        pinned.generation.activated_at
    );
    assert_eq!(manager.active_generation_ordinal(), Some(7));
    assert_eq!(refreshed.routes[&route].id, pinned.routes[&route].id);
    assert_eq!(
        refreshed.routes[&route].targets[0].id,
        pinned.routes[&route].targets[0].id
    );
    let provider = *refreshed.providers.keys().next().unwrap();
    assert!(Arc::ptr_eq(
        &refreshed.transport(provider).unwrap(),
        &pinned.transport(provider).unwrap()
    ));
    assert_eq!(
        pinned.api_keys[&current.lookup_id].digest,
        historical.digest
    );
    assert_eq!(
        pinned.api_keys[&current.lookup_id].scopes,
        historical.scopes
    );
    assert_eq!(
        pinned.api_keys[&current.lookup_id].limits,
        historical.limits
    );
}

#[test]
fn revoked_or_expired_authority_can_be_removed_without_installing_a_generation() {
    let historical = historical_key();
    let manager = installed_manager(historical.clone());
    let pinned = manager.pin();
    let mut expired = historical.clone();
    expired.expires_at = Some(Utc::now() - Duration::hours(1));
    manager
        .refresh_api_keys(BTreeMap::from([(expired.lookup_id.clone(), expired)]))
        .unwrap();
    assert!(
        authorize_api_key(
            &manager.pin().api_keys[&historical.lookup_id],
            None,
            Some(GatewayCapability::Inference),
            GatewayCapability::Inference,
            Utc::now()
        )
        .is_err()
    );

    manager.refresh_api_keys(BTreeMap::new()).unwrap();
    assert!(manager.pin().api_keys.is_empty());
    assert!(pinned.api_keys.contains_key(&historical.lookup_id));
    assert!(
        !manager
            .install(pinned.snapshot.clone(), pinned.transports.clone())
            .unwrap()
    );
    assert!(manager.pin().api_keys.is_empty());
    assert_eq!(manager.active_generation_ordinal(), Some(7));
}

#[test]
fn invalid_authority_refresh_preserves_the_last_successful_bundle() {
    let manager = installed_manager(historical_key());
    let pinned = manager.pin();
    let mismatched_lookup = ApiKeyLookupId::parse("different_lookup").unwrap();
    assert!(
        manager
            .refresh_api_keys(BTreeMap::from([(mismatched_lookup, historical_key())]))
            .is_err()
    );
    assert!(Arc::ptr_eq(&pinned, &manager.pin()));
}

#[test]
fn authority_refresh_does_not_make_an_unloaded_runtime_ready() {
    let manager = Manager::empty();
    let key = historical_key();
    manager
        .refresh_api_keys(BTreeMap::from([(key.lookup_id.clone(), key)]))
        .unwrap();
    assert_eq!(manager.active_generation_ordinal(), None);
    assert_eq!(manager.pin().generation.ordinal, 0);
}
