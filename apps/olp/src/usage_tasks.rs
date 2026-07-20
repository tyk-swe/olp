use std::{
    future::Future,
    panic::AssertUnwindSafe,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};

use futures::future::FutureExt as _;
use tokio_util::{sync::CancellationToken, task::TaskTracker};

#[derive(Clone)]
pub(crate) struct UsageTaskTracker {
    inner: Arc<TrackerInner>,
}

struct TrackerInner {
    tasks: TaskTracker,
    cancellation: CancellationToken,
    closed: Mutex<bool>,
    failed: AtomicBool,
}

impl Default for UsageTaskTracker {
    fn default() -> Self {
        Self {
            inner: Arc::new(TrackerInner {
                tasks: TaskTracker::new(),
                cancellation: CancellationToken::new(),
                closed: Mutex::new(false),
                failed: AtomicBool::new(false),
            }),
        }
    }
}

impl UsageTaskTracker {
    pub(crate) fn spawn<F>(&self, future: F) -> bool
    where
        F: Future<Output = ()> + Send + 'static,
    {
        let closed = self
            .inner
            .closed
            .lock()
            .expect("usage task tracker lock poisoned");
        if *closed {
            self.inner.failed.store(true, Ordering::Release);
            return false;
        }
        let cancellation = self.inner.cancellation.clone();
        let inner = Arc::clone(&self.inner);
        self.inner.tasks.spawn(async move {
            let outcome = AssertUnwindSafe(async move {
                tokio::select! {
                    () = cancellation.cancelled() => false,
                    () = future => true,
                }
            })
            .catch_unwind()
            .await;
            if !matches!(outcome, Ok(true)) {
                inner.failed.store(true, Ordering::Release);
            }
        });
        true
    }

    pub(crate) fn close(&self) {
        let mut closed = self
            .inner
            .closed
            .lock()
            .expect("usage task tracker lock poisoned");
        *closed = true;
        self.inner.tasks.close();
    }

    pub(crate) async fn wait(&self, timeout: Duration) -> bool {
        let completed = tokio::time::timeout(timeout, self.inner.tasks.wait())
            .await
            .is_ok();
        if !completed {
            self.inner.failed.store(true, Ordering::Release);
            self.inner.cancellation.cancel();
            self.inner.tasks.wait().await;
        }
        !self.inner.failed.load(Ordering::Acquire)
    }
}

#[cfg(test)]
mod tests {
    use std::sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    };

    use tokio::sync::oneshot;

    use super::UsageTaskTracker;

    #[tokio::test]
    async fn close_waits_for_registered_work() {
        let tracker = UsageTaskTracker::default();
        let (release, released) = oneshot::channel();
        assert!(tracker.spawn(async move {
            let _ = released.await;
        }));
        tracker.close();
        release.send(()).unwrap();

        assert!(tracker.wait(std::time::Duration::from_secs(1)).await);
    }

    #[tokio::test]
    async fn panic_prevents_a_clean_drain() {
        let tracker = UsageTaskTracker::default();
        assert!(tracker.spawn(async { panic!("producer failed") }));
        tracker.close();

        assert!(!tracker.wait(std::time::Duration::from_secs(1)).await);
    }

    #[tokio::test]
    async fn timeout_aborts_and_drops_registered_work() {
        struct DropSignal(Arc<AtomicBool>);

        impl Drop for DropSignal {
            fn drop(&mut self) {
                self.0.store(true, Ordering::Release);
            }
        }

        let tracker = UsageTaskTracker::default();
        let dropped = Arc::new(AtomicBool::new(false));
        let task_dropped = Arc::clone(&dropped);
        assert!(tracker.spawn(async move {
            let _drop_signal = DropSignal(task_dropped);
            std::future::pending::<()>().await;
        }));
        tracker.close();

        assert!(!tracker.wait(std::time::Duration::ZERO).await);
        assert!(dropped.load(Ordering::Acquire));
    }

    #[tokio::test]
    async fn work_is_rejected_after_close() {
        let tracker = UsageTaskTracker::default();
        tracker.close();

        assert!(!tracker.spawn(async {}));
        assert!(!tracker.wait(std::time::Duration::from_secs(1)).await);
    }
}
