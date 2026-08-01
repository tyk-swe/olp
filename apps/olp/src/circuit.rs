use std::{
    collections::{BTreeMap, BTreeSet},
    sync::{Arc, Mutex},
    time::Duration,
};

use olp_domain::{AttemptFailureClass, TargetId};
use tokio::time::Instant;

const DEFAULT_FAILURE_THRESHOLD: u32 = 5;
const DEFAULT_OPEN_DURATION: Duration = Duration::from_secs(30);

/// Per-gateway target circuit state. Configuration generations stay immutable;
/// this deliberately small, process-local overlay only suppresses targets that
/// are repeatedly failing. A half-open target admits exactly one probe.
#[derive(Clone)]
pub(crate) struct CircuitBreaker {
    inner: Arc<Mutex<BTreeMap<TargetId, CircuitState>>>,
    failure_threshold: u32,
    open_duration: Duration,
}

pub(crate) struct CircuitPermit {
    breaker: CircuitBreaker,
    target: TargetId,
    generation: u64,
    active: bool,
}

impl Drop for CircuitPermit {
    fn drop(&mut self) {
        if self.active {
            self.breaker.record_abandoned(self.target, self.generation);
        }
    }
}

#[derive(Clone, Copy, Debug)]
enum CircuitState {
    Closed {
        consecutive_failures: u32,
        generation: u64,
    },
    Open {
        until: Instant,
        generation: u64,
    },
    HalfOpen {
        lease_until: Instant,
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
            failure_threshold: failure_threshold.max(1),
            open_duration: open_duration.max(Duration::from_millis(1)),
        }
    }

    /// Cheap selection-time check. The actual half-open lease is claimed by
    /// [`Self::try_acquire`] immediately before transport execution.
    pub(crate) fn is_selectable(&self, target: TargetId) -> bool {
        let now = Instant::now();
        let states = self.inner.lock().expect("circuit state lock poisoned");
        match states.get(&target) {
            None | Some(CircuitState::Closed { .. }) => true,
            Some(CircuitState::Open { until, .. }) => now >= *until,
            Some(CircuitState::HalfOpen { lease_until, .. }) => now >= *lease_until,
        }
    }

    /// Claims permission to execute this target. An expired open circuit moves
    /// to half-open and admits one caller; concurrent callers skip it.
    pub(crate) fn try_acquire(
        &self,
        target: TargetId,
        attempt_deadline: Instant,
    ) -> Option<CircuitPermit> {
        let now = Instant::now();
        let mut states = self.inner.lock().expect("circuit state lock poisoned");
        let generation = match states.get(&target).copied() {
            None => 0,
            Some(CircuitState::Closed { generation, .. }) => generation,
            Some(CircuitState::Open { until, generation }) if now >= until => {
                states.insert(
                    target,
                    CircuitState::HalfOpen {
                        lease_until: attempt_deadline,
                        generation,
                    },
                );
                generation
            }
            Some(CircuitState::HalfOpen {
                lease_until,
                generation,
            }) if now >= lease_until => {
                // Recover if a probing request was cancelled before reporting
                // an outcome; otherwise a circuit could remain stuck forever.
                let generation = generation.wrapping_add(1);
                states.insert(
                    target,
                    CircuitState::HalfOpen {
                        lease_until: attempt_deadline,
                        generation,
                    },
                );
                generation
            }
            Some(CircuitState::Open { .. } | CircuitState::HalfOpen { .. }) => return None,
        };
        Some(CircuitPermit {
            breaker: self.clone(),
            target,
            generation,
            active: true,
        })
    }

    pub(crate) fn record_success(&self, mut permit: CircuitPermit) {
        permit.active = false;
        let mut states = self.inner.lock().expect("circuit state lock poisoned");
        let Some(state) = states.get_mut(&permit.target) else {
            return;
        };
        let generation = match *state {
            CircuitState::Closed { generation, .. } if generation == permit.generation => {
                generation
            }
            CircuitState::HalfOpen { generation, .. } if generation == permit.generation => {
                generation.wrapping_add(1)
            }
            _ => return,
        };
        *state = CircuitState::Closed {
            consecutive_failures: 0,
            generation,
        };
    }

    pub(crate) fn retain_targets(&self, live: &BTreeSet<TargetId>) {
        self.inner
            .lock()
            .expect("circuit state lock poisoned")
            .retain(|target, _| live.contains(target));
    }

    pub(crate) fn record_failure(&self, mut permit: CircuitPermit, class: AttemptFailureClass) {
        permit.active = false;
        if !counts_toward_circuit(class) {
            self.record_abandoned(permit.target, permit.generation);
            return;
        }
        let now = Instant::now();
        let mut states = self.inner.lock().expect("circuit state lock poisoned");
        let next = match states.get(&permit.target).copied() {
            Some(CircuitState::HalfOpen { generation, .. }) if generation == permit.generation => {
                CircuitState::Open {
                    until: now + self.open_duration,
                    generation: generation.wrapping_add(1),
                }
            }
            Some(CircuitState::Closed {
                consecutive_failures,
                generation,
            }) if generation == permit.generation => {
                let failures = consecutive_failures.saturating_add(1);
                if failures >= self.failure_threshold {
                    CircuitState::Open {
                        until: now + self.open_duration,
                        generation: generation.wrapping_add(1),
                    }
                } else {
                    CircuitState::Closed {
                        consecutive_failures: failures,
                        generation,
                    }
                }
            }
            None if permit.generation == 0 => {
                if self.failure_threshold == 1 {
                    CircuitState::Open {
                        until: now + self.open_duration,
                        generation: 1,
                    }
                } else {
                    CircuitState::Closed {
                        consecutive_failures: 1,
                        generation: 0,
                    }
                }
            }
            _ => return,
        };
        states.insert(permit.target, next);
    }

    fn record_abandoned(&self, target: TargetId, permit_generation: u64) {
        let now = Instant::now();
        let mut states = self.inner.lock().expect("circuit state lock poisoned");
        let Some(CircuitState::HalfOpen { generation, .. }) = states.get(&target).copied() else {
            return;
        };
        if generation == permit_generation {
            states.insert(
                target,
                CircuitState::Open {
                    until: now + self.open_duration,
                    generation: generation.wrapping_add(1),
                },
            );
        }
    }

    pub(crate) fn open_count(&self) -> usize {
        let now = Instant::now();
        self.inner
            .lock()
            .expect("circuit state lock poisoned")
            .values()
            .filter(|state| match state {
                CircuitState::Open { until, .. } => now < *until,
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
        let deadline = || Instant::now() + Duration::from_secs(1);
        let permit = breaker.try_acquire(target, deadline()).unwrap();
        breaker.record_failure(permit, AttemptFailureClass::Connect);
        let permit = breaker.try_acquire(target, deadline()).unwrap();
        breaker.record_failure(permit, AttemptFailureClass::UpstreamServer);
        assert!(!breaker.is_selectable(target));
        assert!(breaker.try_acquire(target, deadline()).is_none());
        std::thread::sleep(Duration::from_millis(8));
        assert!(breaker.is_selectable(target));
        let permit = breaker.try_acquire(target, deadline()).unwrap();
        assert!(breaker.try_acquire(target, deadline()).is_none());
        breaker.record_success(permit);
        assert!(breaker.try_acquire(target, deadline()).is_some());
    }

    #[test]
    fn stale_probe_cannot_complete_a_new_probe() {
        for stale_succeeds in [true, false] {
            let breaker = CircuitBreaker::new(1, Duration::from_secs(1));
            let target = TargetId::new();
            breaker.inner.lock().unwrap().insert(
                target,
                CircuitState::Open {
                    until: Instant::now(),
                    generation: 1,
                },
            );

            let stale = breaker.try_acquire(target, Instant::now()).unwrap();
            let current = breaker
                .try_acquire(target, Instant::now() + Duration::from_secs(1))
                .unwrap();
            if stale_succeeds {
                breaker.record_success(stale);
            } else {
                breaker.record_failure(stale, AttemptFailureClass::Connect);
            }
            assert!(
                breaker
                    .try_acquire(target, Instant::now() + Duration::from_secs(1))
                    .is_none()
            );
            breaker.record_success(current);
            assert!(breaker.is_selectable(target));
            assert_eq!(breaker.open_count(), 0);
        }
    }

    #[test]
    fn abandoned_half_open_probe_reopens_for_backoff() {
        let breaker = CircuitBreaker::new(1, Duration::from_millis(5));
        let target = TargetId::new();
        breaker.inner.lock().unwrap().insert(
            target,
            CircuitState::Open {
                until: Instant::now(),
                generation: 1,
            },
        );

        drop(
            breaker
                .try_acquire(target, Instant::now() + Duration::from_secs(86_400))
                .unwrap(),
        );
        assert!(!breaker.is_selectable(target));
        std::thread::sleep(Duration::from_millis(8));
        assert!(
            breaker
                .try_acquire(target, Instant::now() + Duration::from_secs(1))
                .is_some()
        );
    }

    #[test]
    fn non_health_failures_do_not_trip_circuit() {
        let breaker = CircuitBreaker::new(1, Duration::from_secs(1));
        let target = TargetId::new();
        let deadline = || Instant::now() + Duration::from_secs(1);
        for class in [
            AttemptFailureClass::UpstreamClient,
            AttemptFailureClass::Protocol,
            AttemptFailureClass::Cancelled,
            AttemptFailureClass::Ambiguous,
            AttemptFailureClass::RateLimit,
        ] {
            let permit = breaker.try_acquire(target, deadline()).unwrap();
            breaker.record_failure(permit, class);
        }
        assert!(breaker.try_acquire(target, deadline()).is_some());
    }

    #[test]
    fn removes_state_for_targets_absent_from_the_installed_generation() {
        let breaker = CircuitBreaker::new(1, Duration::from_secs(1));
        let retained = TargetId::new();
        let removed = TargetId::new();
        let deadline = Instant::now() + Duration::from_secs(1);
        let retained_permit = breaker.try_acquire(retained, deadline).unwrap();
        let removed_permit = breaker.try_acquire(removed, deadline).unwrap();
        breaker.record_failure(retained_permit, AttemptFailureClass::Connect);
        breaker.record_failure(removed_permit, AttemptFailureClass::Connect);
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
