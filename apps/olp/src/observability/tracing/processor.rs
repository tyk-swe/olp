use std::{
    fmt,
    sync::{
        atomic::{AtomicBool, Ordering},
        mpsc as std_mpsc,
    },
    time::Duration,
};

use opentelemetry::Context;
use opentelemetry_sdk::{
    Resource,
    error::{OTelSdkError, OTelSdkResult},
    trace::{Span, SpanData, SpanExporter as _, SpanProcessor},
};
use tokio::{
    sync::mpsc,
    time::{Instant, MissedTickBehavior, interval_at},
};

pub(super) struct BoundedSpanProcessor {
    spans: mpsc::Sender<SpanData>,
    controls: mpsc::UnboundedSender<ControlMessage>,
    shutdown_started: AtomicBool,
}

impl BoundedSpanProcessor {
    pub(super) fn new(
        exporter: super::CountingExporter,
        queue_capacity: usize,
        batch_size: usize,
        delay: Duration,
    ) -> Self {
        assert!(queue_capacity > 0);
        assert!(batch_size > 0);
        assert!(!delay.is_zero());

        let (spans, span_receiver) = mpsc::channel(queue_capacity);
        let (controls, control_receiver) = mpsc::unbounded_channel();
        let worker = Worker {
            exporter,
            spans: span_receiver,
            controls: control_receiver,
            pending: Vec::with_capacity(batch_size),
            batch_size,
            delay,
        };
        let _worker = tokio::spawn(worker.run());

        Self {
            spans,
            controls,
            shutdown_started: AtomicBool::new(false),
        }
    }

    fn send_control(&self, message: ControlMessage) -> OTelSdkResult {
        self.controls
            .send(message)
            .map_err(|_| OTelSdkError::InternalFailure("trace export worker is unavailable".into()))
    }
}

impl fmt::Debug for BoundedSpanProcessor {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("BoundedSpanProcessor")
            .finish_non_exhaustive()
    }
}

impl SpanProcessor for BoundedSpanProcessor {
    fn on_start(&self, _span: &mut Span, _context: &Context) {}

    fn on_end(&self, span: SpanData) {
        if !span.span_context.is_sampled() {
            return;
        }
        if self.spans.try_send(span).is_err() {
            super::record_export_drops(1);
        }
    }

    fn force_flush(&self) -> OTelSdkResult {
        if self.shutdown_started.load(Ordering::Acquire) {
            return Err(OTelSdkError::AlreadyShutdown);
        }
        let (acknowledgment, response) = std_mpsc::channel();
        self.send_control(ControlMessage::ForceFlush(acknowledgment))?;
        response.recv().map_err(|_| {
            OTelSdkError::InternalFailure("trace export worker dropped flush response".into())
        })?
    }

    fn shutdown_with_timeout(&self, timeout: Duration) -> OTelSdkResult {
        if self.shutdown_started.swap(true, Ordering::AcqRel) {
            return Ok(());
        }
        let (acknowledgment, response) = std_mpsc::channel();
        self.send_control(ControlMessage::Shutdown(timeout, acknowledgment))?;
        match response.recv_timeout(timeout) {
            Ok(result) => result,
            Err(std_mpsc::RecvTimeoutError::Timeout) => Err(OTelSdkError::Timeout(timeout)),
            Err(std_mpsc::RecvTimeoutError::Disconnected) => Err(OTelSdkError::InternalFailure(
                "trace export worker dropped shutdown response".into(),
            )),
        }
    }

    fn set_resource(&mut self, resource: &Resource) {
        let _ = self
            .controls
            .send(ControlMessage::SetResource(resource.clone()));
    }
}

enum ControlMessage {
    ForceFlush(std_mpsc::Sender<OTelSdkResult>),
    Shutdown(Duration, std_mpsc::Sender<OTelSdkResult>),
    SetResource(Resource),
}

struct Worker {
    exporter: super::CountingExporter,
    spans: mpsc::Receiver<SpanData>,
    controls: mpsc::UnboundedReceiver<ControlMessage>,
    pending: Vec<SpanData>,
    batch_size: usize,
    delay: Duration,
}

impl Worker {
    async fn run(mut self) {
        let mut ticker = interval_at(Instant::now() + self.delay, self.delay);
        ticker.set_missed_tick_behavior(MissedTickBehavior::Delay);
        loop {
            tokio::select! {
                biased;
                control = self.controls.recv() => {
                    let Some(control) = control else {
                        let _ = self.shutdown_exporter().await;
                        return;
                    };
                    if !self.handle_control(control).await {
                        return;
                    }
                }
                span = self.spans.recv() => {
                    let Some(span) = span else {
                        let _ = self.shutdown_exporter().await;
                        return;
                    };
                    let _ = self.push(span).await;
                }
                _ = ticker.tick() => {
                    let _ = self.export_pending().await;
                }
            }
        }
    }

    async fn handle_control(&mut self, control: ControlMessage) -> bool {
        match control {
            ControlMessage::ForceFlush(acknowledgment) => {
                let result = self.flush_queued().await;
                let _ = acknowledgment.send(result);
                true
            }
            ControlMessage::Shutdown(timeout, acknowledgment) => {
                let budget = timeout.saturating_sub(Duration::from_millis(25));
                let result = match tokio::time::timeout(budget, self.shutdown_exporter()).await {
                    Ok(result) => result,
                    Err(_) => {
                        self.abandon_queued();
                        Err(OTelSdkError::Timeout(timeout))
                    }
                };
                let _ = acknowledgment.send(result);
                false
            }
            ControlMessage::SetResource(resource) => {
                self.exporter.set_resource(&resource);
                true
            }
        }
    }

    async fn push(&mut self, span: SpanData) -> OTelSdkResult {
        self.pending.push(span);
        if self.pending.len() >= self.batch_size {
            self.export_pending().await
        } else {
            Ok(())
        }
    }

    /// Drains `pending` in `batch_size` chunks. Outside shutdown `push` keeps
    /// `pending` at or below one batch, so this is a single export.
    async fn export_pending(&mut self) -> OTelSdkResult {
        while !self.pending.is_empty() {
            self.export_next_batch().await?;
        }
        Ok(())
    }

    async fn flush_queued(&mut self) -> OTelSdkResult {
        let mut result = Ok(());
        let queued = self.spans.len();
        for _ in 0..queued {
            let Some(span) = self.spans.recv().await else {
                break;
            };
            preserve_first_error(&mut result, self.push(span).await);
        }
        preserve_first_error(&mut result, self.export_pending().await);
        preserve_first_error(&mut result, self.exporter.force_flush());
        result
    }

    async fn shutdown_exporter(&mut self) -> OTelSdkResult {
        self.spans.close();
        while let Some(span) = self.spans.recv().await {
            self.pending.push(span);
        }
        let mut result = self.export_pending().await;
        if result.is_err() && !self.pending.is_empty() {
            super::record_export_drops(u64::try_from(self.pending.len()).unwrap_or(u64::MAX));
            self.pending.clear();
        }
        preserve_first_error(&mut result, self.exporter.force_flush());
        preserve_first_error(&mut result, self.exporter.shutdown());
        result
    }

    async fn export_next_batch(&mut self) -> OTelSdkResult {
        let remaining = self
            .pending
            .split_off(self.batch_size.min(self.pending.len()));
        let batch = std::mem::replace(&mut self.pending, remaining);
        self.exporter.export(batch).await
    }

    fn abandon_queued(&mut self) {
        self.spans.close();
        let mut dropped = self.pending.len();
        self.pending.clear();
        while self.spans.try_recv().is_ok() {
            dropped = dropped.saturating_add(1);
        }
        super::record_export_drops(u64::try_from(dropped).unwrap_or(u64::MAX));
    }
}

fn preserve_first_error(result: &mut OTelSdkResult, next: OTelSdkResult) {
    if result.is_ok() {
        *result = next;
    }
}

#[cfg(test)]
mod tests {
    use std::sync::atomic::Ordering;

    use opentelemetry::{
        InstrumentationScope,
        trace::{SpanContext, SpanId, SpanKind, Status, TraceFlags, TraceId, TraceState},
    };
    use opentelemetry_sdk::trace::{SpanData, SpanEvents, SpanLinks, SpanProcessor as _};

    use super::BoundedSpanProcessor;

    fn sampled_span() -> SpanData {
        SpanData {
            span_context: SpanContext::new(
                TraceId::from(1),
                SpanId::from(1),
                TraceFlags::SAMPLED,
                false,
                TraceState::default(),
            ),
            parent_span_id: SpanId::INVALID,
            parent_span_is_remote: false,
            span_kind: SpanKind::Internal,
            name: "test".into(),
            start_time: opentelemetry::time::now(),
            end_time: opentelemetry::time::now(),
            attributes: Vec::new(),
            dropped_attributes_count: 0,
            events: SpanEvents::default(),
            links: SpanLinks::default(),
            status: Status::Unset,
            instrumentation_scope: InstrumentationScope::default(),
        }
    }

    #[test]
    fn full_queue_counts_each_dropped_span_without_blocking() {
        let (spans, _receiver) = tokio::sync::mpsc::channel(1);
        let (controls, _control_receiver) = tokio::sync::mpsc::unbounded_channel();
        let processor = BoundedSpanProcessor {
            spans,
            controls,
            shutdown_started: std::sync::atomic::AtomicBool::new(false),
        };
        let before = super::super::TRACE_EXPORT_DROPPED_TOTAL.load(Ordering::Relaxed);

        processor.on_end(sampled_span());
        processor.on_end(sampled_span());

        let after = super::super::TRACE_EXPORT_DROPPED_TOTAL.load(Ordering::Relaxed);
        assert!(after >= before.saturating_add(1));
    }
}
