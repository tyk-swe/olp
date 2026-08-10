//! Process-local provider-target circuit state.

use std::{
    collections::{BTreeMap, BTreeSet},
    sync::{
        Arc, Mutex,
        atomic::{AtomicU64, Ordering},
    },
    time::{Duration, Instant},
};

use olp_domain::{AttemptFailureClass, TargetId};

const DEFAULT_FAILURE_THRESHOLD: u32 = 5;
const DEFAULT_OPEN_DURATION: Duration = Duration::from_secs(30);

/// Per-gateway target circuit state. Configuration generations stay immutable;
/// this deliberately small, process-local overlay only suppresses targets that
/// are repeatedly failing. A half-open target admits exactly one probe.
#[derive(Clone)]
pub struct CircuitBreaker {
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

impl Default for CircuitBreaker {
    fn default() -> Self {
        Self::new(DEFAULT_FAILURE_THRESHOLD, DEFAULT_OPEN_DURATION)
    }
}

impl CircuitBreaker {
    fn new(failure_threshold: u32, open_duration: Duration) -> Self {
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
        let states = self.inner.lock().expect("circuit state lock poisoned");
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
        let mut states = self.inner.lock().expect("circuit state lock poisoned");
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
        self.inner
            .lock()
            .expect("circuit state lock poisoned")
            .remove(&target);
    }

    /// Releases a half-open probe that ended before the provider was called.
    /// The expired open state keeps the next probe single-flight without
    /// penalizing the target with a fresh recovery interval.
    pub(crate) fn abandon_probe(&self, target: TargetId, permit: CircuitPermit) {
        let Some(probe_generation) = permit.probe_generation else {
            return;
        };
        let mut states = self.inner.lock().expect("circuit state lock poisoned");
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
        self.inner
            .lock()
            .expect("circuit state lock poisoned")
            .retain(|target, _| live.contains(target));
    }

    pub fn record_failure(&self, target: TargetId, class: AttemptFailureClass) {
        if !counts_toward_circuit(class) {
            return;
        }
        let now = Instant::now();
        let mut states = self.inner.lock().expect("circuit state lock poisoned");
        let next = match states.get(&target).copied() {
            Some(CircuitState::HalfOpen { .. } | CircuitState::Open { .. }) => CircuitState::Open {
                until: now + self.open_duration,
            },
            Some(CircuitState::Closed {
                consecutive_failures,
            }) => {
                let failures = consecutive_failures.saturating_add(1);
                if failures >= self.failure_threshold {
                    CircuitState::Open {
                        until: now + self.open_duration,
                    }
                } else {
                    CircuitState::Closed {
                        consecutive_failures: failures,
                    }
                }
            }
            None => {
                if self.failure_threshold == 1 {
                    CircuitState::Open {
                        until: now + self.open_duration,
                    }
                } else {
                    CircuitState::Closed {
                        consecutive_failures: 1,
                    }
                }
            }
        };
        states.insert(target, next);
    }

    pub fn open_count(&self) -> usize {
        let now = Instant::now();
        self.inner
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

    #[test]
    fn opens_half_opens_and_recovers() {
        let breaker = CircuitBreaker::new(2, Duration::from_millis(5));
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
        let breaker = CircuitBreaker::new(1, Duration::from_secs(1));
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
    fn abandoned_half_open_probe_is_immediately_reclaimable() {
        let breaker = CircuitBreaker::default();
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

    #[test]
    fn stale_probe_cannot_abandon_a_newer_lease() {
        let breaker = CircuitBreaker::new(1, Duration::from_secs(1));
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

    #[test]
    fn removes_state_for_targets_absent_from_the_installed_generation() {
        let breaker = CircuitBreaker::new(1, Duration::from_secs(1));
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
