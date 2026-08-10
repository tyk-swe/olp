use std::time::Duration;

use olp_domain::TargetId;
use olp_storage::circuits::{DistributedCircuitBreaker, DistributedCircuitPermit};
use uuid::Uuid;

const RETENTION: Duration = Duration::from_secs(2);

async fn replicas(label: &str) -> (DistributedCircuitBreaker, DistributedCircuitBreaker) {
    let url = std::env::var("OLP_VALKEY_URL")
        .expect("OLP_VALKEY_URL must point to a Valkey test endpoint");
    let namespace = format!("olp:test:circuits:{label}:{}", Uuid::now_v7().simple());
    let first = DistributedCircuitBreaker::connect(&url, &namespace)
        .await
        .unwrap();
    let second = DistributedCircuitBreaker::connect(&url, &namespace)
        .await
        .unwrap();
    (first, second)
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
    let (first, second) = replicas("replicas").await;
    let target = TargetId::new();
    let lease = Duration::from_millis(120);

    // A failure below threshold retains a counter but remains fully closed.
    let ordinary = first.acquire(target, lease, RETENTION).await.unwrap();
    assert_eq!(token(&ordinary), None);
    first
        .record_failure(target, token(&ordinary), 2, lease, RETENTION)
        .await
        .unwrap();
    let ordinary = second.acquire(target, lease, RETENTION).await.unwrap();
    assert_eq!(token(&ordinary), None);

    // Opening in one replica immediately suppresses the other.
    second
        .record_failure(target, token(&ordinary), 2, lease, RETENTION)
        .await
        .unwrap();
    assert!(!first.observe(target).await.unwrap());
    assert_eq!(
        second.acquire(target, lease, RETENTION).await.unwrap(),
        DistributedCircuitPermit::Denied
    );

    tokio::time::sleep(lease + Duration::from_millis(30)).await;
    let (left, right) = tokio::join!(
        first.acquire(target, lease, RETENTION),
        second.acquire(target, lease, RETENTION)
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
        token(&second.acquire(target, lease, RETENTION).await.unwrap()),
        None
    );
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn shorter_later_failure_does_not_shorten_open_deadline() {
    let (first, second) = replicas("retry-after").await;
    let target = TargetId::new();
    let long_open = Duration::from_millis(180);
    let short_open = Duration::from_millis(40);

    let long = first.acquire(target, short_open, RETENTION).await.unwrap();
    let shorter = second.acquire(target, short_open, RETENTION).await.unwrap();
    assert!(
        first
            .record_failure(target, token(&long), 1, long_open, RETENTION)
            .await
            .unwrap()
    );
    assert!(
        second
            .record_failure(target, token(&shorter), 1, short_open, RETENTION)
            .await
            .unwrap()
    );

    tokio::time::sleep(short_open + Duration::from_millis(40)).await;
    assert!(!first.observe(target).await.unwrap());
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn failed_probe_reopens_and_expired_lease_recovers_after_crash() {
    let (first, second) = replicas("leases").await;
    let target = TargetId::new();
    let lease = Duration::from_millis(100);

    let permit = first.acquire(target, lease, RETENTION).await.unwrap();
    first
        .record_failure(target, token(&permit), 1, lease, RETENTION)
        .await
        .unwrap();
    tokio::time::sleep(lease + Duration::from_millis(25)).await;
    let abandoned = first.acquire(target, lease, RETENTION).await.unwrap();
    assert!(token(&abandoned).is_some());
    assert_eq!(
        second.acquire(target, lease, RETENTION).await.unwrap(),
        DistributedCircuitPermit::Denied
    );

    // Simulate the probe owner crashing. Its lease naturally expires.
    tokio::time::sleep(lease + Duration::from_millis(25)).await;
    let recovered = second.acquire(target, lease, RETENTION).await.unwrap();
    assert!(token(&recovered).is_some());
    first
        .record_success(target, token(&abandoned))
        .await
        .unwrap();
    assert!(!first.observe(target).await.unwrap());

    second
        .record_failure(target, token(&recovered), 1, lease, RETENTION)
        .await
        .unwrap();
    assert!(!first.observe(target).await.unwrap());
    assert_eq!(
        first.acquire(target, lease, RETENTION).await.unwrap(),
        DistributedCircuitPermit::Denied
    );
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn expired_probe_success_closes_when_token_has_not_been_replaced() {
    let (first, second) = replicas("slow-success").await;
    let target = TargetId::new();
    let lease = Duration::from_millis(80);

    let permit = first.acquire(target, lease, RETENTION).await.unwrap();
    first
        .record_failure(target, token(&permit), 1, lease, RETENTION)
        .await
        .unwrap();
    tokio::time::sleep(lease + Duration::from_millis(25)).await;
    let probe = first.acquire(target, lease, RETENTION).await.unwrap();
    assert!(token(&probe).is_some());

    tokio::time::sleep(lease + Duration::from_millis(25)).await;
    assert!(first.record_success(target, token(&probe)).await.unwrap());
    assert!(second.observe(target).await.unwrap());
    assert_eq!(
        token(&second.acquire(target, lease, RETENTION).await.unwrap()),
        None
    );
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn expired_probe_failure_reopens_when_token_has_not_been_replaced() {
    let (first, second) = replicas("slow-failure").await;
    let target = TargetId::new();
    let lease = Duration::from_millis(80);

    let permit = first.acquire(target, lease, RETENTION).await.unwrap();
    first
        .record_failure(target, token(&permit), 1, lease, RETENTION)
        .await
        .unwrap();
    tokio::time::sleep(lease + Duration::from_millis(25)).await;
    let probe = first.acquire(target, lease, RETENTION).await.unwrap();
    assert!(token(&probe).is_some());

    tokio::time::sleep(lease + Duration::from_millis(25)).await;
    assert!(
        first
            .record_failure(target, token(&probe), 1, lease, RETENTION)
            .await
            .unwrap()
    );
    assert!(!second.observe(target).await.unwrap());
    assert_eq!(
        second.acquire(target, lease, RETENTION).await.unwrap(),
        DistributedCircuitPermit::Denied
    );
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn stale_probe_success_is_rejected_after_replacement_failure() {
    let (first, second) = replicas("stale-success").await;
    let target = TargetId::new();
    let lease = Duration::from_millis(80);

    let permit = first.acquire(target, lease, RETENTION).await.unwrap();
    first
        .record_failure(target, token(&permit), 1, lease, RETENTION)
        .await
        .unwrap();
    tokio::time::sleep(lease + Duration::from_millis(25)).await;
    let abandoned = first.acquire(target, lease, RETENTION).await.unwrap();
    assert!(token(&abandoned).is_some());

    tokio::time::sleep(lease + Duration::from_millis(25)).await;
    let replacement = second.acquire(target, lease, RETENTION).await.unwrap();
    assert!(token(&replacement).is_some());
    second
        .record_failure(target, token(&replacement), 1, lease, RETENTION)
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
        first.acquire(target, lease, RETENTION).await.unwrap(),
        DistributedCircuitPermit::Denied
    );
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn stale_probe_failure_is_rejected_after_replacement_success() {
    let (first, second) = replicas("stale-failure").await;
    let target = TargetId::new();
    let lease = Duration::from_millis(80);

    let permit = first.acquire(target, lease, RETENTION).await.unwrap();
    first
        .record_failure(target, token(&permit), 1, lease, RETENTION)
        .await
        .unwrap();
    tokio::time::sleep(lease + Duration::from_millis(25)).await;
    let abandoned = first.acquire(target, lease, RETENTION).await.unwrap();
    assert!(token(&abandoned).is_some());

    tokio::time::sleep(lease + Duration::from_millis(25)).await;
    let replacement = second.acquire(target, lease, RETENTION).await.unwrap();
    assert!(token(&replacement).is_some());
    second
        .record_success(target, token(&replacement))
        .await
        .unwrap();

    assert!(
        !first
            .record_failure(target, token(&abandoned), 1, lease, RETENTION)
            .await
            .unwrap()
    );
    assert!(first.observe(target).await.unwrap());
    assert_eq!(
        token(&first.acquire(target, lease, RETENTION).await.unwrap()),
        None
    );
}
