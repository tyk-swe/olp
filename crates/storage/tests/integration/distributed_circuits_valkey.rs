use std::time::Duration;

use olp_domain::TargetId;
use olp_storage::circuits::{DistributedCircuitBreaker, DistributedCircuitPermit};
use uuid::Uuid;

fn valkey_url() -> String {
    std::env::var("OLP_VALKEY_URL").expect("OLP_VALKEY_URL must point to a Valkey test endpoint")
}

fn namespace(label: &str) -> String {
    format!("olp:test:circuits:{label}:{}", Uuid::now_v7().simple())
}

fn token(permit: &DistributedCircuitPermit) -> Option<&str> {
    match permit {
        DistributedCircuitPermit::Acquired { probe_token } => probe_token.as_deref(),
        DistributedCircuitPermit::Denied => panic!("expected acquired circuit permit"),
    }
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn replicas_share_open_state_and_one_recovering_probe() {
    let namespace = namespace("replicas");
    let first = DistributedCircuitBreaker::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let second = DistributedCircuitBreaker::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let target = TargetId::new();
    let lease = Duration::from_millis(120);
    let retention = Duration::from_secs(2);

    // A failure below threshold retains a counter but remains fully closed.
    let ordinary = first.acquire(target, lease, retention).await.unwrap();
    assert_eq!(token(&ordinary), None);
    first
        .record_failure(target, token(&ordinary), 2, lease, retention)
        .await
        .unwrap();
    let ordinary = second.acquire(target, lease, retention).await.unwrap();
    assert_eq!(token(&ordinary), None);

    // Opening in one replica immediately suppresses the other.
    second
        .record_failure(target, token(&ordinary), 2, lease, retention)
        .await
        .unwrap();
    assert!(!first.observe(target).await.unwrap());
    assert_eq!(
        second.acquire(target, lease, retention).await.unwrap(),
        DistributedCircuitPermit::Denied
    );

    tokio::time::sleep(lease + Duration::from_millis(30)).await;
    let (left, right) = tokio::join!(
        first.acquire(target, lease, retention),
        second.acquire(target, lease, retention)
    );
    let permits = [left.unwrap(), right.unwrap()];
    assert_eq!(
        permits
            .iter()
            .filter(|permit| matches!(permit, DistributedCircuitPermit::Acquired { .. }))
            .count(),
        1
    );
    assert_eq!(
        permits
            .iter()
            .filter(|permit| matches!(permit, DistributedCircuitPermit::Denied))
            .count(),
        1
    );
    let probe = permits
        .iter()
        .find(|permit| matches!(permit, DistributedCircuitPermit::Acquired { .. }))
        .unwrap();
    assert!(token(probe).is_some());
    first.record_success(target, token(probe)).await.unwrap();
    assert!(second.observe(target).await.unwrap());
    assert_eq!(
        token(&second.acquire(target, lease, retention).await.unwrap()),
        None
    );
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn failed_probe_reopens_and_expired_lease_recovers_after_crash() {
    let namespace = namespace("leases");
    let first = DistributedCircuitBreaker::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let second = DistributedCircuitBreaker::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let target = TargetId::new();
    let lease = Duration::from_millis(100);
    let retention = Duration::from_secs(2);

    let permit = first.acquire(target, lease, retention).await.unwrap();
    first
        .record_failure(target, token(&permit), 1, lease, retention)
        .await
        .unwrap();
    tokio::time::sleep(lease + Duration::from_millis(25)).await;
    let abandoned = first.acquire(target, lease, retention).await.unwrap();
    assert!(token(&abandoned).is_some());
    assert_eq!(
        second.acquire(target, lease, retention).await.unwrap(),
        DistributedCircuitPermit::Denied
    );

    // Simulate the probe owner crashing. Its lease naturally expires.
    tokio::time::sleep(lease + Duration::from_millis(25)).await;
    let recovered = second.acquire(target, lease, retention).await.unwrap();
    assert!(token(&recovered).is_some());
    first
        .record_success(target, token(&abandoned))
        .await
        .unwrap();
    assert!(!first.observe(target).await.unwrap());

    second
        .record_failure(target, token(&recovered), 1, lease, retention)
        .await
        .unwrap();
    assert!(!first.observe(target).await.unwrap());
    assert_eq!(
        first.acquire(target, lease, retention).await.unwrap(),
        DistributedCircuitPermit::Denied
    );
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn stale_probe_success_is_rejected_after_replacement_failure() {
    let namespace = namespace("stale-success");
    let first = DistributedCircuitBreaker::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let second = DistributedCircuitBreaker::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let target = TargetId::new();
    let lease = Duration::from_millis(80);
    let retention = Duration::from_secs(2);

    let permit = first.acquire(target, lease, retention).await.unwrap();
    first
        .record_failure(target, token(&permit), 1, lease, retention)
        .await
        .unwrap();
    tokio::time::sleep(lease + Duration::from_millis(25)).await;
    let abandoned = first.acquire(target, lease, retention).await.unwrap();
    assert!(token(&abandoned).is_some());

    tokio::time::sleep(lease + Duration::from_millis(25)).await;
    let replacement = second.acquire(target, lease, retention).await.unwrap();
    assert!(token(&replacement).is_some());
    second
        .record_failure(target, token(&replacement), 1, lease, retention)
        .await
        .unwrap();

    assert!(
        !first
            .record_success(target, token(&abandoned))
            .await
            .unwrap()
    );
    assert!(!first.observe(target).await.unwrap());
    assert_eq!(
        first.acquire(target, lease, retention).await.unwrap(),
        DistributedCircuitPermit::Denied
    );
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn stale_probe_failure_is_rejected_after_replacement_success() {
    let namespace = namespace("stale-failure");
    let first = DistributedCircuitBreaker::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let second = DistributedCircuitBreaker::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let target = TargetId::new();
    let lease = Duration::from_millis(80);
    let retention = Duration::from_secs(2);

    let permit = first.acquire(target, lease, retention).await.unwrap();
    first
        .record_failure(target, token(&permit), 1, lease, retention)
        .await
        .unwrap();
    tokio::time::sleep(lease + Duration::from_millis(25)).await;
    let abandoned = first.acquire(target, lease, retention).await.unwrap();
    assert!(token(&abandoned).is_some());

    tokio::time::sleep(lease + Duration::from_millis(25)).await;
    let replacement = second.acquire(target, lease, retention).await.unwrap();
    assert!(token(&replacement).is_some());
    second
        .record_success(target, token(&replacement))
        .await
        .unwrap();

    assert!(
        !first
            .record_failure(target, token(&abandoned), 1, lease, retention)
            .await
            .unwrap()
    );
    assert!(first.observe(target).await.unwrap());
    assert_eq!(
        token(&first.acquire(target, lease, retention).await.unwrap()),
        None
    );
}
