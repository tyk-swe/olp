//! Process-local provider-target circuit state.

use std::collections::BTreeMap;
use std::collections::BTreeSet;
use std::sync::Arc;
use std::sync::Mutex;
use std::sync::MutexGuard;
use std::sync::atomic::AtomicU64;
use std::sync::atomic::Ordering;
use std::time::Duration;
use std::time::Instant;

use crate::ids::TargetId;
use crate::inference::transport::AttemptFailureClass;

const DEFAULT_FAILURE_THRESHOLD: u32 = 5;
const DEFAULT_OPEN_DURATION: Duration = Duration::from_secs(30);

/// Per-gateway target circuit state. Configuration generations stay immutable;
/// this deliberately small, process-local overlay only suppresses targets that
/// are repeatedly failing. A half-open target admits exactly one probe.
#[derive(Clone)]
pub struct Breaker {
    inner: Arc<Mutex<BTreeMap<TargetId, CircuitState>>>,
    next_probe_generation: Arc<AtomicU64>,
    failure_threshold: u32,
    open_duration: Duration,
}

/// Permission to execute a target, including the identity of a half-open probe.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct CircuitPermit {
    probe_generation: Option<u64>,
}

#[derive(Clone, Copy, Debug)]
enum CircuitState {
    Closed {
        consecutive_failures: u32,
    },
    Open {
        until: Instant,
    },
    HalfOpen {
        probe_started: Instant,
        generation: u64,
    },
}

impl Default for Breaker {
    fn default() -> Self {
        Self::new(DEFAULT_FAILURE_THRESHOLD, DEFAULT_OPEN_DURATION)
    }
}

impl Breaker {
    /// Circuit entries are independent, so a panic while the lock was held
    /// leaves nothing to repair; recovering keeps selection serving.
    fn states(&self) -> MutexGuard<'_, BTreeMap<TargetId, CircuitState>> {
        self.inner
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    pub(crate) fn new(failure_threshold: u32, open_duration: Duration) -> Self {
        Self {
            inner: Arc::new(Mutex::new(BTreeMap::new())),
            next_probe_generation: Arc::new(AtomicU64::new(1)),
            failure_threshold: failure_threshold.max(1),
            open_duration: open_duration.max(Duration::from_millis(1)),
        }
    }

    /// Cheap selection-time check. The actual half-open lease is claimed by
    /// [`Self::try_acquire`] immediately before transport execution.
    pub fn is_selectable(&self, target: TargetId) -> bool {
        let now = Instant::now();
        let states = self.states();
        match states.get(&target) {
            None | Some(CircuitState::Closed { .. }) => true,
            Some(CircuitState::Open { until }) => now >= *until,
            Some(CircuitState::HalfOpen { probe_started, .. }) => {
                now.duration_since(*probe_started) >= self.open_duration
            }
        }
    }

    /// Claims permission to execute this target. An expired open circuit moves
    /// to half-open and admits one caller; concurrent callers skip it.
    pub fn try_acquire(&self, target: TargetId) -> bool {
        self.try_acquire_permit(target).is_some()
    }

    pub(crate) fn try_acquire_permit(&self, target: TargetId) -> Option<CircuitPermit> {
        let now = Instant::now();
        let mut states = self.states();
        match states.get(&target).copied() {
            None | Some(CircuitState::Closed { .. }) => Some(CircuitPermit {
                probe_generation: None,
            }),
            Some(CircuitState::Open { until }) if now >= until => {
                let generation = self.next_probe_generation.fetch_add(1, Ordering::Relaxed);
                states.insert(
                    target,
                    CircuitState::HalfOpen {
                        probe_started: now,
                        generation,
                    },
                );
                Some(CircuitPermit {
                    probe_generation: Some(generation),
                })
            }
            Some(CircuitState::HalfOpen { probe_started, .. })
                if now.duration_since(probe_started) >= self.open_duration =>
            {
                // Recover if a probing request was cancelled before reporting
                // an outcome; otherwise a circuit could remain stuck forever.
                let generation = self.next_probe_generation.fetch_add(1, Ordering::Relaxed);
                states.insert(
                    target,
                    CircuitState::HalfOpen {
                        probe_started: now,
                        generation,
                    },
                );
                Some(CircuitPermit {
                    probe_generation: Some(generation),
                })
            }
            Some(CircuitState::Open { .. } | CircuitState::HalfOpen { .. }) => None,
        }
    }

    pub fn record_success(&self, target: TargetId) {
        self.record_success_for_optional_permit(target, None);
    }

    /// Releases a half-open probe that ended before the provider was called.
    /// The expired open state keeps the next probe single-flight without
    /// penalizing the target with a fresh recovery interval.
    pub(crate) fn abandon_probe(&self, target: TargetId, permit: CircuitPermit) {
        let Some(probe_generation) = permit.probe_generation else {
            return;
        };
        let mut states = self.states();
        if matches!(
            states.get(&target),
            Some(CircuitState::HalfOpen { generation, .. }) if *generation == probe_generation
        ) {
            states.insert(
                target,
                CircuitState::Open {
                    until: Instant::now(),
                },
            );
        }
    }

    pub fn retain_targets(&self, live: &BTreeSet<TargetId>) {
        self.states().retain(|target, _| live.contains(target));
    }

    pub fn record_failure(&self, target: TargetId, class: AttemptFailureClass) {
        self.record_failure_for_optional_permit(target, None, class, None);
    }

    pub(crate) fn record_success_for_optional_permit(
        &self,
        target: TargetId,
        permit: Option<&CircuitPermit>,
    ) {
        let mut states = self.states();
        if Self::permit_is_current(&states, target, permit) {
            states.remove(&target);
        }
    }

    pub(crate) fn record_failure_for_optional_permit(
        &self,
        target: TargetId,
        permit: Option<&CircuitPermit>,
        class: AttemptFailureClass,
        retry_after: Option<Duration>,
    ) {
        let now = Instant::now();
        let mut states = self.states();
        if !Self::permit_is_current(&states, target, permit) {
            return;
        }
        if !counts_toward_circuit(class, retry_after) {
            // A credential-scoped outcome says nothing about the endpoint, but
            // it still completes the half-open probe. Release it without a new
            // recovery interval so a sibling credential can probe immediately.
            if permit.is_some_and(|permit| permit.probe_generation.is_some()) {
                states.insert(target, CircuitState::Open { until: now });
            }
            return;
        }
        let failures = match states.get(&target) {
            Some(CircuitState::Closed {
                consecutive_failures,
            }) => consecutive_failures.saturating_add(1),
            Some(CircuitState::Open { .. } | CircuitState::HalfOpen { .. }) => {
                self.failure_threshold
            }
            None => 1,
        };
        let next = if failures >= self.failure_threshold {
            CircuitState::Open {
                until: now + self.open_duration,
            }
        } else {
            CircuitState::Closed {
                consecutive_failures: failures,
            }
        };
        states.insert(target, next);
    }

    fn permit_is_current(
        states: &BTreeMap<TargetId, CircuitState>,
        target: TargetId,
        permit: Option<&CircuitPermit>,
    ) -> bool {
        let Some(permit) = permit else {
            return true;
        };
        match permit.probe_generation {
            Some(generation) => matches!(
                states.get(&target),
                Some(CircuitState::HalfOpen {
                    generation: current,
                    ..
                }) if *current == generation
            ),
            None => !matches!(
                states.get(&target),
                Some(CircuitState::Open { .. } | CircuitState::HalfOpen { .. })
            ),
        }
    }

    pub fn open_count(&self) -> usize {
        let now = Instant::now();
        self.states()
            .values()
            .filter(|state| match state {
                CircuitState::Open { until } => now < *until,
                CircuitState::HalfOpen { .. } => true,
                CircuitState::Closed { .. } => false,
            })
            .count()
    }
}

/// Authentication and quota failures belong to credential health. Only
/// connection, timeout, and server failures affect the endpoint circuit.
/// Classes that do not count still resolve an outstanding half-open probe
/// (see `record_failure_for_optional_permit`) so they never block siblings.
const fn counts_toward_circuit(class: AttemptFailureClass, _retry_after: Option<Duration>) -> bool {
    match class {
        AttemptFailureClass::Connect
        | AttemptFailureClass::Timeout
        | AttemptFailureClass::UpstreamServer => true,
        AttemptFailureClass::RateLimit => false,
        AttemptFailureClass::UpstreamClient
        | AttemptFailureClass::Protocol
        | AttemptFailureClass::Cancelled
        | AttemptFailureClass::Ambiguous => false,
    }
}

#[cfg(test)]
mod tests {
    use crate::inference::circuit::*;

    #[test]
    fn opens_half_opens_and_recovers() {
        let breaker = Breaker::new(2, Duration::from_millis(5));
        let target = TargetId::new();
        assert!(breaker.try_acquire(target));
        breaker.record_failure(target, AttemptFailureClass::Connect);
        assert!(breaker.try_acquire(target));
        breaker.record_failure(target, AttemptFailureClass::UpstreamServer);
        assert!(!breaker.is_selectable(target));
        assert!(!breaker.try_acquire(target));
        std::thread::sleep(Duration::from_millis(8));
        assert!(breaker.is_selectable(target));
        assert!(breaker.try_acquire(target));
        assert!(!breaker.try_acquire(target));
        breaker.record_success(target);
        assert!(breaker.try_acquire(target));
    }

    #[test]
    fn client_protocol_and_ambiguous_failures_do_not_trip_circuit() {
        let breaker = Breaker::new(1, Duration::from_secs(1));
        let target = TargetId::new();
        for class in [
            AttemptFailureClass::UpstreamClient,
            AttemptFailureClass::Protocol,
            AttemptFailureClass::Cancelled,
            AttemptFailureClass::Ambiguous,
        ] {
            breaker.record_failure(target, class);
            assert!(breaker.try_acquire(target));
        }
    }

    #[test]
    fn bare_upstream_rate_limits_never_open_the_shared_target_circuit() {
        let breaker = Breaker::new(1, Duration::from_secs(30));
        let target = TargetId::new();
        for _ in 0..10 {
            breaker.record_failure(target, AttemptFailureClass::RateLimit);
        }
        assert!(breaker.is_selectable(target));
        assert!(breaker.try_acquire(target));
        assert_eq!(breaker.open_count(), 0);
    }

    #[test]
    fn upstream_rate_limits_leave_endpoint_available_for_other_credentials() {
        let breaker = Breaker::new(2, Duration::from_secs(30));
        let target = TargetId::new();
        let rate_limited = |breaker: &Breaker| {
            breaker.record_failure_for_optional_permit(
                target,
                None,
                AttemptFailureClass::RateLimit,
                Some(Duration::from_secs(1)),
            );
        };
        rate_limited(&breaker);
        assert!(breaker.is_selectable(target));
        assert!(breaker.try_acquire(target));
        rate_limited(&breaker);
        assert!(breaker.is_selectable(target));
        assert!(breaker.try_acquire(target));
        assert_eq!(breaker.open_count(), 0);
    }

    #[test]
    fn abandoned_half_open_probe_is_immediately_reclaimable() {
        let breaker = Breaker::default();
        let target = TargetId::new();
        breaker
            .inner
            .lock()
            .expect("circuit state lock poisoned")
            .insert(
                target,
                CircuitState::Open {
                    until: Instant::now(),
                },
            );
        let permit = breaker
            .try_acquire_permit(target)
            .expect("expired open circuit admits a probe");

        breaker.abandon_probe(target, permit);

        assert!(breaker.is_selectable(target));
        assert!(breaker.try_acquire(target));
        assert!(!breaker.try_acquire(target));
    }

    /// A credential-only outcome (401/429/local quota) does not count against
    /// the endpoint, but it must still complete the half-open probe so a
    /// sibling credential slot can probe the same target immediately.
    #[test]
    fn credential_scoped_failure_releases_the_half_open_probe_without_penalty() {
        for (class, retry_after) in [
            (
                AttemptFailureClass::RateLimit,
                Some(Duration::from_secs(30)),
            ),
            (AttemptFailureClass::RateLimit, None),
            (AttemptFailureClass::UpstreamClient, None),
        ] {
            let breaker = Breaker::new(1, Duration::from_secs(30));
            let target = TargetId::new();
            breaker
                .inner
                .lock()
                .expect("circuit state lock poisoned")
                .insert(
                    target,
                    CircuitState::Open {
                        until: Instant::now(),
                    },
                );
            let probe = breaker
                .try_acquire_permit(target)
                .expect("expired open circuit admits a probe");
            assert!(!breaker.try_acquire(target), "probe is single-flight");

            breaker.record_failure_for_optional_permit(target, Some(&probe), class, retry_after);

            assert!(breaker.is_selectable(target), "{class:?} must not reopen");
            let sibling = breaker
                .try_acquire_permit(target)
                .expect("sibling credential can probe immediately");
            assert_ne!(sibling.probe_generation, probe.probe_generation);
            assert!(!breaker.try_acquire(target));
        }
    }

    #[test]
    fn stale_credential_scoped_failure_cannot_release_a_newer_probe() {
        let breaker = Breaker::new(1, Duration::from_millis(5));
        let target = TargetId::new();
        breaker
            .inner
            .lock()
            .expect("circuit state lock poisoned")
            .insert(
                target,
                CircuitState::Open {
                    until: Instant::now(),
                },
            );
        let stale = breaker
            .try_acquire_permit(target)
            .expect("expired open circuit admits a probe");
        std::thread::sleep(Duration::from_millis(8));
        let current = breaker
            .try_acquire_permit(target)
            .expect("a probe past open_duration is superseded");

        breaker.record_failure_for_optional_permit(
            target,
            Some(&stale),
            AttemptFailureClass::RateLimit,
            None,
        );
        assert!(!breaker.try_acquire(target));
        breaker.record_failure_for_optional_permit(
            target,
            Some(&current),
            AttemptFailureClass::UpstreamClient,
            None,
        );
        assert!(breaker.try_acquire(target));
    }

    #[test]
    fn stale_probe_cannot_abandon_a_newer_lease() {
        let breaker = Breaker::new(1, Duration::from_secs(1));
        let target = TargetId::new();
        breaker
            .inner
            .lock()
            .expect("circuit state lock poisoned")
            .insert(
                target,
                CircuitState::Open {
                    until: Instant::now(),
                },
            );
        let stale_permit = breaker
            .try_acquire_permit(target)
            .expect("expired open circuit admits a probe");
        if let Some(CircuitState::HalfOpen { probe_started, .. }) = breaker
            .inner
            .lock()
            .expect("circuit state lock poisoned")
            .get_mut(&target)
        {
            *probe_started = Instant::now() - breaker.open_duration;
        }
        let current_permit = breaker
            .try_acquire_permit(target)
            .expect("stale probe lease can be replaced");

        breaker.abandon_probe(target, stale_permit);

        assert!(!breaker.try_acquire(target));
        breaker.abandon_probe(target, current_permit);
        assert!(breaker.try_acquire(target));
    }

    /// A streaming probe routinely outlives `open_duration`, at which point a
    /// newer probe supersedes it. If the stale one then reported success while
    /// dropping its permit, it closed a breaker the newer probe had just proved
    /// is still broken.
    #[test]
    fn a_stale_probe_outcome_cannot_close_a_breaker_a_newer_probe_holds() {
        let breaker = Breaker::new(1, Duration::from_millis(5));
        let target = TargetId::new();
        breaker
            .inner
            .lock()
            .expect("circuit state lock poisoned")
            .insert(
                target,
                CircuitState::Open {
                    until: Instant::now(),
                },
            );
        let stale = breaker
            .try_acquire_permit(target)
            .expect("expired open circuit admits a probe");
        std::thread::sleep(Duration::from_millis(8));
        let current = breaker
            .try_acquire_permit(target)
            .expect("a probe past open_duration is superseded");

        breaker.record_success_for_optional_permit(target, Some(&stale));
        assert!(
            !breaker.try_acquire(target),
            "the stale probe must not release the newer probe's lease"
        );

        // The permit-free call is what used to happen and would have closed it.
        breaker.record_success_for_optional_permit(target, Some(&current));
        assert!(breaker.try_acquire(target));
    }

    #[test]
    fn removes_state_for_targets_absent_from_the_installed_generation() {
        let breaker = Breaker::new(1, Duration::from_secs(1));
        let retained = TargetId::new();
        let removed = TargetId::new();
        breaker.record_failure(retained, AttemptFailureClass::Connect);
        breaker.record_failure(removed, AttemptFailureClass::Connect);
        assert_eq!(breaker.open_count(), 2);

        breaker.retain_targets(&BTreeSet::from([retained]));

        assert_eq!(breaker.open_count(), 1);
        assert!(!breaker.is_selectable(retained));
        assert!(breaker.is_selectable(removed));
        assert_eq!(
            breaker
                .inner
                .lock()
                .expect("circuit state lock poisoned")
                .len(),
            1
        );
    }
}
