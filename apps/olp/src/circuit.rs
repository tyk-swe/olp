use std::{
    collections::BTreeMap,
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
            Some(CircuitState::HalfOpen { probe_started }) => {
                now.duration_since(*probe_started) >= self.open_duration
            }
        }
    }

    /// Claims permission to execute this target. An expired open circuit moves
    /// to half-open and admits one caller; concurrent callers skip it.
    pub(crate) fn try_acquire(&self, target: TargetId) -> bool {
        let now = Instant::now();
        let mut states = self.inner.lock().expect("circuit state lock poisoned");
        match states.get(&target).copied() {
            None | Some(CircuitState::Closed { .. }) => true,
            Some(CircuitState::Open { until }) if now >= until => {
                states.insert(target, CircuitState::HalfOpen { probe_started: now });
                true
            }
            Some(CircuitState::HalfOpen { probe_started })
                if now.duration_since(probe_started) >= self.open_duration =>
            {
                // Recover if a probing request was cancelled before reporting
                // an outcome; otherwise a circuit could remain stuck forever.
                states.insert(target, CircuitState::HalfOpen { probe_started: now });
                true
            }
            Some(CircuitState::Open { .. } | CircuitState::HalfOpen { .. }) => false,
        }
    }

    pub(crate) fn record_success(&self, target: TargetId) {
        self.inner
            .lock()
            .expect("circuit state lock poisoned")
            .remove(&target);
    }

    pub(crate) fn record_failure(&self, target: TargetId, class: AttemptFailureClass) {
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
}
