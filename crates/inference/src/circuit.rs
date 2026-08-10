//! Provider-target circuit state with optional distributed coordination.

use std::{
    collections::{BTreeMap, BTreeSet},
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

use arc_swap::ArcSwapOption;
use futures::{StreamExt, stream};
use olp_domain::{AttemptFailureClass, TargetId};
use olp_storage::circuits::{DistributedCircuitBreaker, DistributedCircuitPermit};

const DEFAULT_FAILURE_THRESHOLD: u32 = 5;
const DEFAULT_OPEN_DURATION: Duration = Duration::from_secs(30);
const MAX_RETRY_AFTER: Duration = Duration::from_secs(5 * 60);
const DEGRADED_WARNING_INTERVAL: Duration = Duration::from_secs(30);
const DISTRIBUTED_OPERATION_TIMEOUT: Duration = Duration::from_millis(250);

/// Permission returned by the authoritative check immediately before provider
/// transport. It also identifies an owned local half-open probe.
#[derive(Debug)]
pub struct CircuitPermit {
    target: TargetId,
    source: PermitSource,
    local_probe_started: Option<Instant>,
}

#[derive(Debug)]
enum PermitSource {
    Local,
    Distributed {
        adapter: Arc<DistributedCircuitBreaker>,
        probe_token: Option<String>,
    },
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
    degraded_operations: AtomicU64,
    last_warning_ms: AtomicU64,
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
    record_on_drop: bool,
}

impl PendingLocalFailure<'_> {
    fn record(mut self) {
        self.record_on_drop = false;
        self.breaker
            .local_record_failure(self.permit, self.open_duration);
    }

    fn disarm(mut self) {
        self.record_on_drop = false;
    }
}

impl Drop for PendingLocalFailure<'_> {
    fn drop(&mut self) {
        if self.record_on_drop {
            self.breaker
                .local_record_failure(self.permit, self.open_duration);
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
                degraded_operations: AtomicU64::new(0),
                last_warning_ms: AtomicU64::new(0),
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
        self.note_degraded("distributed circuit store is unavailable");
    }

    /// Selection-time observation is a hint only. The final acquisition must
    /// still be performed immediately before transport.
    pub async fn is_selectable(&self, target: TargetId) -> bool {
        let Some(distributed) = self.inner.distributed.load_full() else {
            return self.local_is_selectable(target);
        };
        match tokio::time::timeout(DISTRIBUTED_OPERATION_TIMEOUT, distributed.observe(target)).await
        {
            Ok(Ok(selectable)) => {
                self.mark_healthy();
                selectable
            }
            Ok(Err(error)) => {
                self.note_degraded_error("observe", &error);
                self.local_is_selectable(target)
            }
            Err(_) => {
                self.note_degraded("observe timed out");
                self.local_is_selectable(target)
            }
        }
    }

    /// Observes a route's targets before deterministic ordering. Results are
    /// hints: acquisition remains authoritative because state can change after
    /// this method returns. Valkey lookups are bounded and do not spawn tasks.
    pub async fn selectable_targets(
        &self,
        targets: impl IntoIterator<Item = TargetId>,
    ) -> BTreeSet<TargetId> {
        // Own the identifiers across the bounded concurrent observations so
        // handler futures remain `Send` regardless of the source iterator.
        let targets = targets.into_iter().collect::<Vec<_>>();
        if self.inner.distributed.load().is_none() {
            return targets
                .into_iter()
                .filter(|target| self.local_is_selectable(*target))
                .collect();
        }
        stream::iter(targets)
            .map(|target| async move { self.is_selectable(target).await.then_some(target) })
            .buffer_unordered(16)
            .filter_map(std::future::ready)
            .collect()
            .await
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
                    let local_probe_started = if probe_token.is_some() {
                        Some(self.local_start_distributed_probe(target))
                    } else {
                        None
                    };
                    return Some(CircuitPermit {
                        target,
                        source: PermitSource::Distributed {
                            adapter: distributed,
                            probe_token,
                        },
                        local_probe_started,
                    });
                }
                Ok(Err(error)) => self.note_degraded_error("acquire", &error),
                Err(_) => self.note_degraded("acquire timed out"),
            }
        }
        self.local_try_acquire(target)
            .map(|local_probe_started| CircuitPermit {
                target,
                source: PermitSource::Local,
                local_probe_started,
            })
    }

    pub async fn record_success(&self, permit: &CircuitPermit) {
        let mut update_local = true;
        if let PermitSource::Distributed {
            adapter,
            probe_token,
        } = &permit.source
        {
            match tokio::time::timeout(
                DISTRIBUTED_OPERATION_TIMEOUT,
                adapter.record_success(permit.target, probe_token.as_deref()),
            )
            .await
            {
                Ok(Ok(applied)) => {
                    self.mark_healthy();
                    update_local = applied;
                }
                Ok(Err(error)) => self.note_degraded_error("record_success", &error),
                Err(_) => self.note_degraded("record_success timed out"),
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
        if let PermitSource::Distributed {
            adapter,
            probe_token,
        } = &permit.source
        {
            // Preserve local circuit accuracy even if the request task is
            // cancelled while awaiting the shared Valkey update below.
            let pending_local = PendingLocalFailure {
                breaker: self,
                permit,
                open_duration,
                record_on_drop: true,
            };
            match tokio::time::timeout(
                DISTRIBUTED_OPERATION_TIMEOUT,
                adapter.record_failure(
                    permit.target,
                    probe_token.as_deref(),
                    self.inner.failure_threshold,
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
                    self.note_degraded_error("record_failure", &error);
                    pending_local.record();
                }
                Err(_) => {
                    self.note_degraded("record_failure timed out");
                    pending_local.record();
                }
            }
        } else {
            self.local_record_failure(permit, open_duration);
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

    pub fn degraded_operations(&self) -> u64 {
        self.inner.degraded_operations.load(Ordering::Relaxed)
    }

    fn local_is_selectable(&self, target: TargetId) -> bool {
        let now = Instant::now();
        let states = self
            .inner
            .local
            .lock()
            .expect("circuit state lock poisoned");
        match states.get(&target) {
            None | Some(CircuitState::Closed { .. }) => true,
            Some(CircuitState::Open { until }) => now >= *until,
            Some(CircuitState::HalfOpen { probe_started }) => {
                now.duration_since(*probe_started) >= self.inner.open_duration
            }
        }
    }

    fn local_try_acquire(&self, target: TargetId) -> Option<Option<Instant>> {
        let now = Instant::now();
        let mut states = self
            .inner
            .local
            .lock()
            .expect("circuit state lock poisoned");
        match states.get(&target).copied() {
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

    fn local_start_distributed_probe(&self, target: TargetId) -> Instant {
        let probe_started = Instant::now();
        self.inner
            .local
            .lock()
            .expect("circuit state lock poisoned")
            .insert(target, CircuitState::HalfOpen { probe_started });
        probe_started
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

    fn local_record_failure(&self, permit: &CircuitPermit, open_duration: Duration) {
        let now = Instant::now();
        let mut states = self
            .inner
            .local
            .lock()
            .expect("circuit state lock poisoned");
        if !local_permit_owns_state(&states, permit) {
            return;
        }
        let next = match states.get(&permit.target).copied() {
            Some(CircuitState::HalfOpen { .. } | CircuitState::Open { .. }) => CircuitState::Open {
                until: now + open_duration,
            },
            Some(CircuitState::Closed {
                consecutive_failures,
            }) => {
                let failures = consecutive_failures.saturating_add(1);
                if failures >= self.inner.failure_threshold {
                    CircuitState::Open {
                        until: now + open_duration,
                    }
                } else {
                    CircuitState::Closed {
                        consecutive_failures: failures,
                    }
                }
            }
            None if self.inner.failure_threshold == 1 => CircuitState::Open {
                until: now + open_duration,
            },
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

    fn note_degraded_error(&self, operation: &'static str, error: &impl std::fmt::Display) {
        self.note_degraded_with(operation, Some(error));
    }

    fn note_degraded(&self, operation: &'static str) {
        self.note_degraded_with::<olp_storage::circuits::CircuitStoreError>(operation, None);
    }

    fn note_degraded_with<E: std::fmt::Display>(&self, operation: &'static str, error: Option<&E>) {
        self.inner
            .distributed_degraded
            .store(true, Ordering::Release);
        self.inner
            .degraded_operations
            .fetch_add(1, Ordering::Relaxed);
        let now_ms = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis()
            .try_into()
            .unwrap_or(u64::MAX);
        let interval_ms = u64::try_from(DEGRADED_WARNING_INTERVAL.as_millis()).unwrap_or(u64::MAX);
        let previous = self.inner.last_warning_ms.load(Ordering::Relaxed);
        if now_ms.saturating_sub(previous) >= interval_ms
            && self
                .inner
                .last_warning_ms
                .compare_exchange(previous, now_ms, Ordering::AcqRel, Ordering::Relaxed)
                .is_ok()
        {
            tracing::warn!(
                circuit_state = "local_fallback",
                operation,
                error = error.map(ToString::to_string),
                "distributed circuit coordination degraded; using process-local protection"
            );
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
        assert!(breaker.degraded_operations() > 0);
        assert!(breaker.acquire(target).await.is_none());
    }

    #[test]
    fn pending_local_failure_records_when_dropped() {
        let breaker = CircuitBreaker::new(1, Duration::from_secs(1));
        let target = TargetId::new();
        let permit = CircuitPermit {
            target,
            source: PermitSource::Local,
            local_probe_started: None,
        };
        drop(PendingLocalFailure {
            breaker: &breaker,
            permit: &permit,
            open_duration: Duration::from_secs(1),
            record_on_drop: true,
        });
        assert!(!breaker.local_is_selectable(target));
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
        breaker.local_record_failure(&replacement, Duration::from_secs(1));
        breaker.local_record_success(&stale_success);
        assert!(breaker.local_try_acquire(stale_success.target).is_none());

        let stale_failure = distributed_probe_after_open(&breaker, TargetId::new()).await;
        tokio::time::sleep(Duration::from_millis(55)).await;
        let replacement = local_replacement_probe(&breaker, stale_failure.target);
        breaker.local_record_success(&replacement);
        breaker.local_record_failure(&stale_failure, Duration::from_secs(1));
        assert_eq!(breaker.local_try_acquire(stale_failure.target), Some(None));
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
                &probe.source,
                PermitSource::Distributed {
                    probe_token: Some(_),
                    ..
                }
            ),
            "expected distributed half-open probe permit"
        );
        assert!(probe.local_probe_started.is_some());
        probe
    }

    fn local_replacement_probe(breaker: &CircuitBreaker, target: TargetId) -> CircuitPermit {
        let local_probe_started = breaker
            .local_try_acquire(target)
            .expect("local fallback should allow replacement probe");
        assert!(local_probe_started.is_some());
        CircuitPermit {
            target,
            source: PermitSource::Local,
            local_probe_started,
        }
    }
}
