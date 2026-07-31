use std::{
    collections::{BTreeMap, BTreeSet},
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

use olp_domain::{AttemptFailureClass, TargetId};

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

#[derive(Clone, Copy, Debug)]
enum CircuitState {
    Closed { consecutive_failures: u32 },
    Open { until: Instant },
    HalfOpen { probe_started: Instant },
}

pub(crate) struct CircuitPermit {
    breaker: CircuitBreaker,
    target: TargetId,
    probe_started: Option<Instant>,
}

impl CircuitPermit {
    pub(crate) fn record_success(&mut self) {
        self.breaker
            .record_success_for(self.target, self.probe_started);
        self.probe_started = None;
    }

    pub(crate) fn record_failure(&mut self, class: AttemptFailureClass) {
        self.breaker
            .record_failure_for(self.target, class, self.probe_started);
        self.probe_started = None;
    }
}

impl Drop for CircuitPermit {
    fn drop(&mut self) {
        let Some(probe_started) = self.probe_started else {
            return;
        };
        // A destructor must not panic: a poisoned lock during unwinding would
        // abort the process instead of failing one request.
        let mut states = self
            .breaker
            .inner
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if matches!(
            states.get(&self.target),
            Some(CircuitState::HalfOpen {
                probe_started: active
            }) if *active == probe_started
        ) {
            states.insert(
                self.target,
                CircuitState::Open {
                    until: Instant::now() + self.breaker.open_duration,
                },
            );
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
            Some(CircuitState::Open { until }) => now >= *until,
            Some(CircuitState::HalfOpen { .. }) => false,
        }
    }

    /// Claims permission to execute this target. An expired open circuit moves
    /// to half-open and admits one caller; concurrent callers skip it.
    pub(crate) fn try_acquire(&self, target: TargetId) -> Option<CircuitPermit> {
        let now = Instant::now();
        let mut states = self.inner.lock().expect("circuit state lock poisoned");
        match states.get(&target).copied() {
            None | Some(CircuitState::Closed { .. }) => Some(CircuitPermit {
                breaker: self.clone(),
                target,
                probe_started: None,
            }),
            Some(CircuitState::Open { until }) if now >= until => {
                states.insert(target, CircuitState::HalfOpen { probe_started: now });
                Some(CircuitPermit {
                    breaker: self.clone(),
                    target,
                    probe_started: Some(now),
                })
            }
            Some(CircuitState::Open { .. } | CircuitState::HalfOpen { .. }) => None,
        }
    }

    fn record_success_for(&self, target: TargetId, probe_started: Option<Instant>) {
        let mut states = self.inner.lock().expect("circuit state lock poisoned");
        match (states.get(&target), probe_started) {
            (None | Some(CircuitState::Closed { .. }), None) => {}
            (
                Some(CircuitState::HalfOpen {
                    probe_started: active,
                }),
                Some(owner),
            ) if *active == owner => {}
            _ => return,
        }
        states.remove(&target);
    }

    pub(crate) fn retain_targets(&self, live: &BTreeSet<TargetId>) {
        self.inner
            .lock()
            .expect("circuit state lock poisoned")
            .retain(|target, _| live.contains(target));
    }

    #[cfg(test)]
    pub(crate) fn record_failure(&self, target: TargetId, class: AttemptFailureClass) {
        self.record_failure_for(target, class, None);
    }

    fn record_failure_for(
        &self,
        target: TargetId,
        class: AttemptFailureClass,
        probe_started: Option<Instant>,
    ) {
        if !counts_toward_circuit(class) {
            let mut states = self.inner.lock().expect("circuit state lock poisoned");
            if matches!(
                states.get(&target),
                Some(CircuitState::HalfOpen {
                    probe_started: active
                }) if Some(*active) == probe_started
            ) {
                if matches!(
                    class,
                    AttemptFailureClass::Cancelled | AttemptFailureClass::Ambiguous
                ) {
                    states.insert(
                        target,
                        CircuitState::Open {
                            until: Instant::now() + self.open_duration,
                        },
                    );
                } else {
                    states.remove(&target);
                }
            }
            return;
        }
        let now = Instant::now();
        let mut states = self.inner.lock().expect("circuit state lock poisoned");
        let next = match states.get(&target).copied() {
            Some(CircuitState::HalfOpen {
                probe_started: active,
            }) if Some(active) != probe_started => return,
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

    pub(crate) fn open_count(&self) -> usize {
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
        assert!(breaker.try_acquire(target).is_some());
        breaker.record_failure(target, AttemptFailureClass::Connect);
        assert!(breaker.try_acquire(target).is_some());
        breaker.record_failure(target, AttemptFailureClass::UpstreamServer);
        assert!(!breaker.is_selectable(target));
        assert!(breaker.try_acquire(target).is_none());
        std::thread::sleep(Duration::from_millis(8));
        assert!(breaker.is_selectable(target));
        let mut probe = breaker.try_acquire(target).unwrap();
        assert!(breaker.try_acquire(target).is_none());
        probe.record_success();
        drop(probe);
        assert!(breaker.try_acquire(target).is_some());
    }

    #[test]
    fn client_rate_limit_protocol_and_ambiguous_failures_do_not_trip_circuit() {
        let breaker = CircuitBreaker::new(1, Duration::from_secs(1));
        let target = TargetId::new();
        for class in [
            AttemptFailureClass::UpstreamClient,
            AttemptFailureClass::RateLimit,
            AttemptFailureClass::Protocol,
            AttemptFailureClass::Cancelled,
            AttemptFailureClass::Ambiguous,
        ] {
            breaker.record_failure(target, class);
            assert!(breaker.try_acquire(target).is_some());
        }

        let half_open = CircuitBreaker::new(1, Duration::from_millis(1));
        half_open.record_failure(target, AttemptFailureClass::Connect);
        std::thread::sleep(Duration::from_millis(2));
        let mut probe = half_open.try_acquire(target).unwrap();
        probe.record_failure(AttemptFailureClass::RateLimit);
        drop(probe);
        assert_eq!(half_open.open_count(), 0);
    }

    #[test]
    fn a_live_half_open_probe_is_never_joined_and_cancellation_reopens_it() {
        let breaker = CircuitBreaker::new(1, Duration::from_millis(5));
        let target = TargetId::new();
        breaker.record_failure(target, AttemptFailureClass::Connect);
        std::thread::sleep(Duration::from_millis(8));
        let probe = breaker.try_acquire(target).unwrap();
        std::thread::sleep(Duration::from_millis(8));
        assert!(breaker.try_acquire(target).is_none());
        drop(probe);
        assert!(!breaker.is_selectable(target));
    }

    #[test]
    fn uncertain_half_open_failures_reopen_the_circuit() {
        for class in [
            AttemptFailureClass::Cancelled,
            AttemptFailureClass::Ambiguous,
        ] {
            let breaker = CircuitBreaker::new(1, Duration::from_millis(5));
            let target = TargetId::new();
            breaker.record_failure(target, AttemptFailureClass::Connect);
            std::thread::sleep(Duration::from_millis(8));

            let mut probe = breaker.try_acquire(target).unwrap();
            probe.record_failure(class);
            drop(probe);

            assert!(!breaker.is_selectable(target));
            assert!(breaker.try_acquire(target).is_none());
            std::thread::sleep(Duration::from_millis(8));
            assert!(
                breaker.try_acquire(target).is_some(),
                "the unresolved probe must become retryable"
            );
        }
    }

    #[test]
    fn stale_attempt_outcomes_cannot_release_a_half_open_probe() {
        let breaker = CircuitBreaker::new(1, Duration::from_millis(5));
        let target = TargetId::new();
        let mut stale = breaker.try_acquire(target).unwrap();
        breaker.record_failure(target, AttemptFailureClass::Connect);
        std::thread::sleep(Duration::from_millis(8));
        let mut probe = breaker.try_acquire(target).unwrap();

        stale.record_success();
        assert!(breaker.try_acquire(target).is_none());
        probe.record_success();
        assert!(breaker.try_acquire(target).is_some());
    }

    #[test]
    fn stale_success_cannot_close_a_newer_open_circuit() {
        let breaker = CircuitBreaker::new(1, Duration::from_secs(1));
        let target = TargetId::new();
        let mut stale = breaker.try_acquire(target).unwrap();
        let mut failing = breaker.try_acquire(target).unwrap();

        failing.record_failure(AttemptFailureClass::Connect);
        stale.record_success();

        assert!(!breaker.is_selectable(target));
        assert!(breaker.try_acquire(target).is_none());
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
