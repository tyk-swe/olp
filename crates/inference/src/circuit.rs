//! Provider-target circuit state with optional distributed coordination.

use std::{
    collections::{BTreeMap, BTreeSet},
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
    time::{Duration, Instant},
};

use arc_swap::ArcSwapOption;
use futures::{StreamExt, stream};
use olp_domain::{AttemptFailureClass, TargetId};
use olp_storage::circuits::{
    CircuitStoreError, DistributedCircuitBreaker, DistributedCircuitPermit,
};

const DEFAULT_FAILURE_THRESHOLD: u32 = 5;
const DEFAULT_OPEN_DURATION: Duration = Duration::from_secs(30);
const MAX_RETRY_AFTER: Duration = Duration::from_secs(5 * 60);
const DISTRIBUTED_OPERATION_TIMEOUT: Duration = Duration::from_millis(250);

/// Permission returned by the authoritative check immediately before provider
/// transport. It also identifies an owned local half-open probe.
#[derive(Debug)]
pub struct CircuitPermit {
    target: TargetId,
    distributed: Option<DistributedPermit>,
    local_probe_started: Option<Instant>,
}

#[derive(Debug)]
struct DistributedPermit {
    adapter: Arc<DistributedCircuitBreaker>,
    probe_token: Option<String>,
}

/// Target circuit facade. The process-local implementation remains active as
/// bounded protection whenever shared Valkey state is not installed or an
/// operation on it fails.
#[derive(Clone)]
pub struct CircuitBreaker {
    inner: Arc<Inner>,
}

struct Inner {
    local: Mutex<BTreeMap<TargetId, CircuitState>>,
    distributed: ArcSwapOption<DistributedCircuitBreaker>,
    distributed_configured: AtomicBool,
    distributed_degraded: AtomicBool,
    degradation_events: AtomicU64,
    failure_threshold: u32,
    open_duration: Duration,
    state_retention: Duration,
}

#[derive(Clone, Copy, Debug)]
enum CircuitState {
    Closed { consecutive_failures: u32 },
    Open { until: Instant },
    HalfOpen { probe_started: Instant },
}

struct PendingLocalFailure<'a> {
    breaker: &'a CircuitBreaker,
    permit: &'a CircuitPermit,
    open_duration: Duration,
    force_open: bool,
    record_on_drop: bool,
}

impl PendingLocalFailure<'_> {
    fn record(mut self) {
        self.record_on_drop = false;
        self.breaker
            .local_record_failure(self.permit, self.open_duration, self.force_open);
    }

    fn disarm(mut self) {
        self.record_on_drop = false;
    }
}

impl Drop for PendingLocalFailure<'_> {
    fn drop(&mut self) {
        if self.record_on_drop {
            self.breaker
                .local_record_failure(self.permit, self.open_duration, self.force_open);
        }
    }
}

impl Default for CircuitBreaker {
    fn default() -> Self {
        Self::new(DEFAULT_FAILURE_THRESHOLD, DEFAULT_OPEN_DURATION)
    }
}

impl CircuitBreaker {
    fn new(failure_threshold: u32, open_duration: Duration) -> Self {
        let open_duration = open_duration.max(Duration::from_millis(1));
        Self {
            inner: Arc::new(Inner {
                local: Mutex::new(BTreeMap::new()),
                distributed: ArcSwapOption::empty(),
                distributed_configured: AtomicBool::new(false),
                distributed_degraded: AtomicBool::new(false),
                degradation_events: AtomicU64::new(0),
                failure_threshold: failure_threshold.max(1),
                open_duration,
                state_retention: open_duration
                    .saturating_mul(10)
                    .max(Duration::from_secs(300)),
            }),
        }
    }

    /// Installs a healthy shared store. Replacements are atomic for requests.
    pub fn install_distributed(&self, distributed: DistributedCircuitBreaker) {
        self.inner.distributed.store(Some(Arc::new(distributed)));
        self.inner
            .distributed_configured
            .store(true, Ordering::Release);
        self.inner
            .distributed_degraded
            .store(false, Ordering::Release);
    }

    /// Marks distributed coordination as configured but currently unavailable.
    pub fn mark_distributed_unavailable(&self) {
        self.inner.distributed.store(None);
        self.inner
            .distributed_configured
            .store(true, Ordering::Release);
        self.note_degraded("distributed circuit store is unavailable", None);
    }

    /// Selection-time observation is a hint only. The final acquisition must
    /// still be performed immediately before transport.
    pub async fn is_selectable(&self, target: TargetId) -> bool {
        self.selectable_targets([target]).await.contains(&target)
    }

    /// Observes a route's targets before deterministic ordering. Results are
    /// hints: acquisition remains authoritative because state can change after
    /// this method returns. Valkey lookups share one route-level timeout and do
    /// not spawn tasks.
    pub async fn selectable_targets(
        &self,
        targets: impl IntoIterator<Item = TargetId>,
    ) -> BTreeSet<TargetId> {
        // Own the identifiers across the bounded concurrent observations so
        // handler futures remain `Send` regardless of the source iterator.
        let targets = targets.into_iter().collect::<Vec<_>>();
        let Some(distributed) = self.inner.distributed.load_full() else {
            return self.local_selectable_targets(&targets);
        };
        if self.inner.distributed_degraded.load(Ordering::Acquire) {
            return self.local_selectable_targets(&targets);
        }

        match tokio::time::timeout(DISTRIBUTED_OPERATION_TIMEOUT, async {
            let mut selectable = BTreeSet::new();
            let mut observations = stream::iter(targets.iter().copied())
                .map(|target| {
                    let distributed = Arc::clone(&distributed);
                    async move { (target, distributed.observe(target).await) }
                })
                .buffer_unordered(16);

            while let Some((target, result)) = observations.next().await {
                match result {
                    Ok(true) => {
                        selectable.insert(target);
                    }
                    Ok(false) => {}
                    Err(error) => return Err(error),
                }
            }
            Ok::<_, CircuitStoreError>(selectable)
        })
        .await
        {
            Ok(Ok(selectable)) => {
                self.mark_healthy();
                selectable
            }
            Ok(Err(error)) => {
                self.note_degraded("observe", Some(&error));
                self.local_selectable_targets(&targets)
            }
            Err(_) => {
                self.note_degraded("observe route timed out", None);
                self.local_selectable_targets(&targets)
            }
        }
    }

    /// Performs the authoritative attempt acquisition. Valkey errors degrade
    /// explicitly to the bounded local implementation instead of failing the
    /// inference request.
    pub async fn acquire(&self, target: TargetId) -> Option<CircuitPermit> {
        if let Some(distributed) = self.inner.distributed.load_full() {
            match tokio::time::timeout(
                DISTRIBUTED_OPERATION_TIMEOUT,
                distributed.acquire(target, self.inner.open_duration, self.inner.state_retention),
            )
            .await
            {
                Ok(Ok(DistributedCircuitPermit::Denied)) => {
                    self.mark_healthy();
                    return None;
                }
                Ok(Ok(DistributedCircuitPermit::Acquired { probe_token })) => {
                    self.mark_healthy();
                    let local_probe_started = self.local_acquire(target, probe_token.is_some())?;
                    return Some(CircuitPermit {
                        target,
                        distributed: Some(DistributedPermit {
                            adapter: distributed,
                            probe_token,
                        }),
                        local_probe_started,
                    });
                }
                Ok(Err(error)) => self.note_degraded("acquire", Some(&error)),
                Err(_) => self.note_degraded("acquire timed out", None),
            }
        }
        self.local_acquire(target, false)
            .map(|local_probe_started| CircuitPermit {
                target,
                distributed: None,
                local_probe_started,
            })
    }

    pub async fn record_success(&self, permit: &CircuitPermit) {
        let mut update_local = true;
        if let Some(distributed) = &permit.distributed {
            match tokio::time::timeout(
                DISTRIBUTED_OPERATION_TIMEOUT,
                distributed
                    .adapter
                    .record_success(permit.target, distributed.probe_token.as_deref()),
            )
            .await
            {
                Ok(Ok(applied)) => {
                    self.mark_healthy();
                    update_local = applied;
                }
                Ok(Err(error)) => self.note_degraded("record_success", Some(&error)),
                Err(_) => self.note_degraded("record_success timed out", None),
            }
        }
        if update_local {
            self.local_record_success(permit);
        }
    }

    pub async fn record_failure(
        &self,
        permit: &CircuitPermit,
        class: AttemptFailureClass,
        retry_after: Option<Duration>,
    ) {
        if !counts_toward_circuit(class) {
            return;
        }
        let open_duration = retry_after
            .map(|duration| duration.max(Duration::from_millis(1)).min(MAX_RETRY_AFTER))
            .unwrap_or(self.inner.open_duration);
        let force_open = retry_after.is_some();
        if let Some(distributed) = &permit.distributed {
            // Preserve local circuit accuracy even if the request task is
            // cancelled while awaiting the shared Valkey update below.
            let pending_local = PendingLocalFailure {
                breaker: self,
                permit,
                open_duration,
                force_open,
                record_on_drop: true,
            };
            let failure_threshold = if force_open {
                1
            } else {
                self.inner.failure_threshold
            };
            match tokio::time::timeout(
                DISTRIBUTED_OPERATION_TIMEOUT,
                distributed.adapter.record_failure(
                    permit.target,
                    distributed.probe_token.as_deref(),
                    failure_threshold,
                    open_duration,
                    self.inner
                        .state_retention
                        .max(open_duration.saturating_mul(2)),
                ),
            )
            .await
            {
                Ok(Ok(applied)) => {
                    self.mark_healthy();
                    if applied {
                        pending_local.record();
                    } else {
                        pending_local.disarm();
                    }
                }
                Ok(Err(error)) => {
                    self.note_degraded("record_failure", Some(&error));
                    pending_local.record();
                }
                Err(_) => {
                    self.note_degraded("record_failure timed out", None);
                    pending_local.record();
                }
            }
        } else {
            self.local_record_failure(permit, open_duration, force_open);
        }
    }

    pub fn retain_targets(&self, live: &BTreeSet<TargetId>) {
        self.inner
            .local
            .lock()
            .expect("circuit state lock poisoned")
            .retain(|target, _| live.contains(target));
    }

    /// Exact process-local open/half-open count. Distributed state deliberately
    /// is not scanned on the request or metrics path.
    pub fn local_open_count(&self) -> usize {
        let now = Instant::now();
        self.inner
            .local
            .lock()
            .expect("circuit state lock poisoned")
            .values()
            .filter(|state| match state {
                CircuitState::Open { until } => now < *until,
                CircuitState::HalfOpen { .. } => true,
                CircuitState::Closed { .. } => false,
            })
            .count()
    }

    pub fn distributed_configured(&self) -> bool {
        self.inner.distributed_configured.load(Ordering::Acquire)
    }

    pub fn distributed_available(&self) -> bool {
        self.inner.distributed.load().is_some()
            && !self.inner.distributed_degraded.load(Ordering::Acquire)
    }

    /// Returns `None` when no adapter is installed and otherwise checks the
    /// current adapter without exposing it to delivery code.
    pub async fn ping_distributed(&self) -> Option<bool> {
        let distributed = self.inner.distributed.load_full()?;
        let healthy = distributed.ping().await.is_ok();
        if healthy {
            self.mark_healthy();
        }
        Some(healthy)
    }

    pub fn degradation_events(&self) -> u64 {
        self.inner.degradation_events.load(Ordering::Relaxed)
    }

    fn local_selectable_targets(&self, targets: &[TargetId]) -> BTreeSet<TargetId> {
        let now = Instant::now();
        let states = self
            .inner
            .local
            .lock()
            .expect("circuit state lock poisoned");
        targets
            .iter()
            .copied()
            .filter(|target| {
                local_state_is_selectable(
                    states.get(target).copied(),
                    now,
                    self.inner.open_duration,
                )
            })
            .collect()
    }

    fn local_acquire(&self, target: TargetId, force_probe: bool) -> Option<Option<Instant>> {
        let now = Instant::now();
        let mut states = self
            .inner
            .local
            .lock()
            .expect("circuit state lock poisoned");
        match states.get(&target).copied() {
            None | Some(CircuitState::Closed { .. }) if force_probe => {
                states.insert(target, CircuitState::HalfOpen { probe_started: now });
                Some(Some(now))
            }
            None | Some(CircuitState::Closed { .. }) => Some(None),
            Some(CircuitState::Open { until }) if now >= until => {
                states.insert(target, CircuitState::HalfOpen { probe_started: now });
                Some(Some(now))
            }
            Some(CircuitState::HalfOpen { probe_started })
                if now.duration_since(probe_started) >= self.inner.open_duration =>
            {
                states.insert(target, CircuitState::HalfOpen { probe_started: now });
                Some(Some(now))
            }
            Some(CircuitState::Open { .. } | CircuitState::HalfOpen { .. }) => None,
        }
    }

    fn local_record_success(&self, permit: &CircuitPermit) {
        let mut states = self
            .inner
            .local
            .lock()
            .expect("circuit state lock poisoned");
        if local_permit_owns_state(&states, permit) {
            states.remove(&permit.target);
        }
    }

    fn local_record_failure(
        &self,
        permit: &CircuitPermit,
        open_duration: Duration,
        force_open: bool,
    ) {
        let now = Instant::now();
        let mut states = self
            .inner
            .local
            .lock()
            .expect("circuit state lock poisoned");
        if !local_permit_owns_state(&states, permit) {
            return;
        }
        let open_until = now + open_duration;
        let next = match states.get(&permit.target).copied() {
            Some(CircuitState::Open { until }) => CircuitState::Open {
                until: until.max(open_until),
            },
            Some(CircuitState::HalfOpen { .. }) => CircuitState::Open { until: open_until },
            Some(CircuitState::Closed {
                consecutive_failures,
            }) => {
                let failures = consecutive_failures.saturating_add(1);
                if force_open || failures >= self.inner.failure_threshold {
                    CircuitState::Open { until: open_until }
                } else {
                    CircuitState::Closed {
                        consecutive_failures: failures,
                    }
                }
            }
            None if force_open || self.inner.failure_threshold == 1 => {
                CircuitState::Open { until: open_until }
            }
            None => CircuitState::Closed {
                consecutive_failures: 1,
            },
        };
        states.insert(permit.target, next);
    }

    fn mark_healthy(&self) {
        self.inner
            .distributed_degraded
            .store(false, Ordering::Release);
    }

    fn note_degraded(&self, operation: &'static str, error: Option<&dyn std::fmt::Display>) {
        if self.inner.distributed_degraded.swap(true, Ordering::AcqRel) {
            return;
        }
        self.inner
            .degradation_events
            .fetch_add(1, Ordering::Relaxed);
        tracing::warn!(
            circuit_state = "local_fallback",
            operation,
            error = error.map(ToString::to_string),
            "distributed circuit coordination degraded; using process-local protection"
        );
    }
}

fn local_state_is_selectable(
    state: Option<CircuitState>,
    now: Instant,
    probe_timeout: Duration,
) -> bool {
    match state {
        None | Some(CircuitState::Closed { .. }) => true,
        Some(CircuitState::Open { until }) => now >= until,
        Some(CircuitState::HalfOpen { probe_started }) => {
            now.duration_since(probe_started) >= probe_timeout
        }
    }
}

fn local_permit_owns_state(
    states: &BTreeMap<TargetId, CircuitState>,
    permit: &CircuitPermit,
) -> bool {
    match permit.local_probe_started {
        Some(owned) => matches!(
            states.get(&permit.target),
            Some(CircuitState::HalfOpen { probe_started }) if *probe_started == owned
        ),
        None => true,
    }
}

const fn counts_toward_circuit(class: AttemptFailureClass) -> bool {
    matches!(
        class,
        AttemptFailureClass::Connect
            | AttemptFailureClass::Timeout
            | AttemptFailureClass::RateLimit
            | AttemptFailureClass::UpstreamServer
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn opens_half_opens_and_recovers() {
        let breaker = CircuitBreaker::new(2, Duration::from_millis(5));
        let target = TargetId::new();
        let permit = breaker.acquire(target).await.unwrap();
        breaker
            .record_failure(&permit, AttemptFailureClass::Connect, None)
            .await;
        let permit = breaker.acquire(target).await.unwrap();
        breaker
            .record_failure(&permit, AttemptFailureClass::UpstreamServer, None)
            .await;
        assert!(!breaker.is_selectable(target).await);
        assert!(breaker.acquire(target).await.is_none());
        tokio::time::sleep(Duration::from_millis(8)).await;
        assert!(breaker.is_selectable(target).await);
        let permit = breaker.acquire(target).await.unwrap();
        assert!(breaker.acquire(target).await.is_none());
        breaker.record_success(&permit).await;
        assert!(breaker.acquire(target).await.is_some());
    }

    #[tokio::test]
    async fn client_protocol_and_ambiguous_failures_do_not_trip_circuit() {
        let breaker = CircuitBreaker::new(1, Duration::from_secs(1));
        let target = TargetId::new();
        for class in [
            AttemptFailureClass::UpstreamClient,
            AttemptFailureClass::Protocol,
            AttemptFailureClass::Cancelled,
            AttemptFailureClass::Ambiguous,
        ] {
            let permit = breaker.acquire(target).await.unwrap();
            breaker.record_failure(&permit, class, None).await;
            assert!(breaker.acquire(target).await.is_some());
        }
    }

    #[tokio::test]
    async fn retry_after_is_bounded() {
        let breaker = CircuitBreaker::new(1, Duration::from_millis(2));
        let target = TargetId::new();
        let permit = breaker.acquire(target).await.unwrap();
        breaker
            .record_failure(
                &permit,
                AttemptFailureClass::RateLimit,
                Some(Duration::from_secs(60 * 60)),
            )
            .await;
        let state = breaker.inner.local.lock().unwrap()[&target];
        let CircuitState::Open { until } = state else {
            panic!("expected open circuit");
        };
        let remaining = until.saturating_duration_since(Instant::now());
        assert!(remaining <= MAX_RETRY_AFTER);
        assert!(remaining > Duration::from_secs(299));

        let shorter = TargetId::new();
        let permit = breaker.acquire(shorter).await.unwrap();
        breaker
            .record_failure(
                &permit,
                AttemptFailureClass::RateLimit,
                Some(Duration::from_millis(20)),
            )
            .await;
        let state = breaker.inner.local.lock().unwrap()[&shorter];
        let CircuitState::Open { until } = state else {
            panic!("expected open circuit");
        };
        assert!(until.saturating_duration_since(Instant::now()) <= Duration::from_millis(20));
    }

    #[tokio::test]
    async fn retry_after_opens_before_failure_threshold() {
        let breaker = CircuitBreaker::new(5, Duration::from_secs(1));
        let target = TargetId::new();
        let permit = breaker.acquire(target).await.unwrap();

        breaker
            .record_failure(
                &permit,
                AttemptFailureClass::RateLimit,
                Some(Duration::from_millis(50)),
            )
            .await;

        assert!(!breaker.is_selectable(target).await);
        assert!(breaker.acquire(target).await.is_none());
    }

    #[tokio::test]
    async fn later_failure_does_not_shorten_existing_open_deadline() {
        let breaker = CircuitBreaker::new(1, Duration::from_millis(5));
        let target = TargetId::new();
        let long = breaker.acquire(target).await.unwrap();
        let shorter = breaker.acquire(target).await.unwrap();

        breaker
            .record_failure(
                &long,
                AttemptFailureClass::RateLimit,
                Some(Duration::from_millis(80)),
            )
            .await;
        let CircuitState::Open { until: first_until } =
            breaker.inner.local.lock().unwrap()[&target]
        else {
            panic!("expected open circuit");
        };

        breaker
            .record_failure(&shorter, AttemptFailureClass::UpstreamServer, None)
            .await;
        let CircuitState::Open {
            until: second_until,
        } = breaker.inner.local.lock().unwrap()[&target]
        else {
            panic!("expected open circuit");
        };
        assert!(second_until >= first_until);
    }

    #[tokio::test]
    async fn expired_local_probe_cannot_overwrite_replacement_probe() {
        let breaker = CircuitBreaker::new(1, Duration::from_millis(5));
        let target = TargetId::new();
        let permit = breaker.acquire(target).await.unwrap();
        breaker
            .record_failure(&permit, AttemptFailureClass::Connect, None)
            .await;
        tokio::time::sleep(Duration::from_millis(8)).await;
        let abandoned = breaker.acquire(target).await.unwrap();
        tokio::time::sleep(Duration::from_millis(8)).await;
        let replacement = breaker.acquire(target).await.unwrap();
        breaker.record_success(&abandoned).await;
        assert!(breaker.acquire(target).await.is_none());
        breaker.record_success(&replacement).await;
        assert!(breaker.acquire(target).await.is_some());
    }

    #[tokio::test]
    async fn removes_state_for_targets_absent_from_the_installed_generation() {
        let breaker = CircuitBreaker::new(1, Duration::from_secs(1));
        let retained = TargetId::new();
        let removed = TargetId::new();
        for target in [retained, removed] {
            let permit = breaker.acquire(target).await.unwrap();
            breaker
                .record_failure(&permit, AttemptFailureClass::Connect, None)
                .await;
        }
        assert_eq!(breaker.local_open_count(), 2);
        breaker.retain_targets(&BTreeSet::from([retained]));
        assert_eq!(breaker.local_open_count(), 1);
        assert!(!breaker.is_selectable(retained).await);
        assert!(breaker.is_selectable(removed).await);
    }

    #[tokio::test]
    async fn configured_valkey_outage_falls_back_to_bounded_local_state() {
        let breaker = CircuitBreaker::new(1, Duration::from_secs(1));
        breaker.mark_distributed_unavailable();
        let target = TargetId::new();
        let permit = breaker.acquire(target).await.unwrap();
        breaker
            .record_failure(&permit, AttemptFailureClass::Connect, None)
            .await;
        assert!(breaker.distributed_configured());
        assert!(!breaker.distributed_available());
        assert!(breaker.degradation_events() > 0);
        assert!(breaker.acquire(target).await.is_none());
    }

    #[test]
    fn distributed_closed_permit_respects_local_fallback_open() {
        let breaker = CircuitBreaker::new(1, Duration::from_secs(1));
        let target = TargetId::new();
        let open_until = Instant::now() + Duration::from_secs(30);
        breaker
            .inner
            .local
            .lock()
            .unwrap()
            .insert(target, CircuitState::Open { until: open_until });

        assert!(breaker.local_acquire(target, false).is_none());
        let CircuitState::Open { until } = breaker.inner.local.lock().unwrap()[&target] else {
            panic!("expected open circuit");
        };
        assert_eq!(until, open_until);
    }

    #[test]
    fn pending_local_failure_records_when_dropped() {
        let breaker = CircuitBreaker::new(1, Duration::from_secs(1));
        let target = TargetId::new();
        let permit = CircuitPermit {
            target,
            distributed: None,
            local_probe_started: None,
        };
        drop(PendingLocalFailure {
            breaker: &breaker,
            permit: &permit,
            open_duration: Duration::from_secs(1),
            force_open: false,
            record_on_drop: true,
        });
        assert!(
            !breaker
                .local_selectable_targets(&[target])
                .contains(&target)
        );
    }

    #[tokio::test]
    #[ignore = "requires Valkey in OLP_VALKEY_URL"]
    async fn stale_distributed_probe_cannot_mutate_replacement_local_probe_on_fallback() {
        let url = std::env::var("OLP_VALKEY_URL").expect("OLP_VALKEY_URL must be set");
        let namespace = format!(
            "olp:test:inference-circuit-fallback:{}",
            TargetId::new().as_uuid()
        );
        let breaker = CircuitBreaker::new(1, Duration::from_millis(40));
        breaker.install_distributed(
            DistributedCircuitBreaker::connect(&url, &namespace)
                .await
                .unwrap(),
        );

        let stale_success = distributed_probe_after_open(&breaker, TargetId::new()).await;
        tokio::time::sleep(Duration::from_millis(55)).await;
        let replacement = local_replacement_probe(&breaker, stale_success.target);
        breaker.local_record_failure(&replacement, Duration::from_secs(1), false);
        breaker.local_record_success(&stale_success);
        assert!(breaker.local_acquire(stale_success.target, false).is_none());

        let stale_failure = distributed_probe_after_open(&breaker, TargetId::new()).await;
        tokio::time::sleep(Duration::from_millis(55)).await;
        let replacement = local_replacement_probe(&breaker, stale_failure.target);
        breaker.local_record_success(&replacement);
        breaker.local_record_failure(&stale_failure, Duration::from_secs(1), false);
        assert_eq!(
            breaker.local_acquire(stale_failure.target, false),
            Some(None)
        );
    }

    #[tokio::test]
    #[ignore = "requires Valkey in OLP_VALKEY_URL"]
    async fn independent_breakers_coordinate_through_valkey() {
        let url = std::env::var("OLP_VALKEY_URL").expect("OLP_VALKEY_URL must be set");
        let namespace = format!("olp:test:inference-circuit:{}", TargetId::new().as_uuid());
        let first = CircuitBreaker::new(1, Duration::from_millis(80));
        let second = CircuitBreaker::new(1, Duration::from_millis(80));
        first.install_distributed(
            DistributedCircuitBreaker::connect(&url, &namespace)
                .await
                .unwrap(),
        );
        second.install_distributed(
            DistributedCircuitBreaker::connect(&url, &namespace)
                .await
                .unwrap(),
        );
        let target = TargetId::new();
        let permit = first.acquire(target).await.unwrap();
        first
            .record_failure(&permit, AttemptFailureClass::Connect, None)
            .await;
        assert!(second.acquire(target).await.is_none());

        tokio::time::sleep(Duration::from_millis(110)).await;
        let (left, right) = tokio::join!(first.acquire(target), second.acquire(target));
        assert!(left.is_some() ^ right.is_some());
        let probe = left.or(right).unwrap();
        first.record_success(&probe).await;
        assert!(second.acquire(target).await.is_some());
    }

    async fn distributed_probe_after_open(
        breaker: &CircuitBreaker,
        target: TargetId,
    ) -> CircuitPermit {
        let permit = breaker.acquire(target).await.unwrap();
        breaker
            .record_failure(&permit, AttemptFailureClass::Connect, None)
            .await;
        tokio::time::sleep(Duration::from_millis(55)).await;
        let probe = breaker.acquire(target).await.unwrap();
        assert!(
            matches!(
                &probe.distributed,
                Some(DistributedPermit {
                    probe_token: Some(_),
                    ..
                })
            ),
            "expected distributed half-open probe permit"
        );
        assert!(probe.local_probe_started.is_some());
        probe
    }

    fn local_replacement_probe(breaker: &CircuitBreaker, target: TargetId) -> CircuitPermit {
        let local_probe_started = breaker
            .local_acquire(target, false)
            .expect("local fallback should allow replacement probe");
        assert!(local_probe_started.is_some());
        CircuitPermit {
            target,
            distributed: None,
            local_probe_started,
        }
    }
}
