use rust_decimal::Decimal;
use tracing::{Span, field};

use crate::inference::failover::attempt_failure_name;

use crate::domain::{
    canonical::identity::{OperationKind, Surface},
    ids::RouteSlug,
    ports::TransportError,
    routing::selection::AttemptPlan,
};

pub const OTEL_TARGET: &str = "olp.telemetry";

pub const REQUEST_ATTRIBUTE_KEYS: &[&str] = &[
    "olp.request_id",
    "olp.surface",
    "olp.operation",
    "olp.route_slug",
    "olp.key_id",
    "olp.installation_id",
    "olp.generation",
    "olp.status",
    "olp.error_class",
    "olp.attempt_count",
    "olp.time_to_first_byte_ms",
    "olp.total_duration_ms",
    "olp.cancelled",
];

pub const ATTEMPT_ATTRIBUTE_KEYS: &[&str] = &[
    "olp.provider_kind",
    "olp.provider_revision",
    "olp.model",
    "olp.outcome_class",
    "olp.upstream_status_class",
    "olp.usage.input_tokens",
    "olp.usage.output_tokens",
    "olp.usage.cached_input_tokens",
    "olp.usage.media_units",
    "olp.pricing_provenance",
];

#[derive(Clone)]
pub struct RequestTrace {
    span: Span,
    propagate_upstream: bool,
    record_request: bool,
}

impl RequestTrace {
    #[must_use]
    pub const fn new(span: Span, propagate_upstream: bool) -> Self {
        Self {
            span,
            propagate_upstream,
            record_request: true,
        }
    }

    #[must_use]
    pub fn attempts_only(&self) -> Self {
        Self {
            span: self.span.clone(),
            propagate_upstream: self.propagate_upstream,
            record_request: false,
        }
    }

    #[must_use]
    pub(in crate::inference) const fn propagate_upstream(&self) -> bool {
        self.propagate_upstream
    }

    pub(in crate::inference) fn record_inference_context(
        &self,
        surface: Surface,
        operation: OperationKind,
        route_slug: &RouteSlug,
        key_id: uuid::Uuid,
        generation: uuid::Uuid,
    ) {
        if !self.record_request {
            return;
        }
        self.span.record("olp.surface", field::display(surface));
        self.span.record("olp.operation", field::display(operation));
        self.span
            .record("olp.route_slug", field::display(route_slug));
        self.span.record("olp.key_id", field::display(key_id));
        self.span
            .record("olp.generation", field::display(generation));
    }

    pub(in crate::inference) fn record_session_context(
        &self,
        operation: OperationKind,
        route_slug: &RouteSlug,
        generation: uuid::Uuid,
    ) {
        if !self.record_request {
            return;
        }
        self.span.record("olp.operation", field::display(operation));
        self.span
            .record("olp.route_slug", field::display(route_slug));
        self.span
            .record("olp.generation", field::display(generation));
    }

    pub(in crate::inference) fn record_terminal(
        &self,
        status_code: Option<u16>,
        error_class: Option<&str>,
        attempt_count: usize,
        first_byte_ms: Option<u64>,
        total_duration_ms: u64,
    ) {
        if !self.record_request {
            return;
        }
        if let Some(status_code) = status_code {
            self.span.record("olp.status", status_code);
        }
        if let Some(error_class) = error_class {
            self.span.record("olp.error_class", error_class);
        }
        self.span.record(
            "olp.attempt_count",
            u64::try_from(attempt_count).unwrap_or(u64::MAX),
        );
        if let Some(first_byte_ms) = first_byte_ms {
            self.span.record("olp.time_to_first_byte_ms", first_byte_ms);
        }
        self.span.record("olp.total_duration_ms", total_duration_ms);
        if error_class == Some("client_cancelled") {
            self.span.record("olp.cancelled", true);
        }
    }

    pub(in crate::inference) fn attempt(&self, plan: &AttemptPlan) -> AttemptTrace {
        let span = tracing::info_span!(
            target: OTEL_TARGET,
            parent: &self.span,
            "attempt",
            "otel.kind" = "client",
            "olp.provider_kind" = %plan.provider_kind,
            "olp.provider_revision" = field::Empty,
            "olp.model" = %plan.upstream_model,
            "olp.outcome_class" = field::Empty,
            "olp.upstream_status_class" = field::Empty,
            "olp.usage.input_tokens" = field::Empty,
            "olp.usage.output_tokens" = field::Empty,
            "olp.usage.cached_input_tokens" = field::Empty,
            "olp.usage.media_units" = field::Empty,
            "olp.pricing_provenance" = field::Empty,
        );
        if let Some(revision) = plan.provider_revision_id {
            span.record("olp.provider_revision", field::display(revision));
        }
        AttemptTrace {
            span,
            completed: false,
        }
    }
}

pub(in crate::inference) struct AttemptTrace {
    span: Span,
    completed: bool,
}

impl AttemptTrace {
    #[must_use]
    pub(in crate::inference) fn span(&self) -> Span {
        self.span.clone()
    }

    pub(in crate::inference) fn record_transport_failure(&mut self, error: &TransportError) {
        self.finish(
            attempt_failure_name(error.class),
            status_class(error.upstream.status),
        );
    }

    pub(in crate::inference) fn record_usage(
        &self,
        input_tokens: Option<i64>,
        output_tokens: Option<i64>,
        cached_input_tokens: Option<i64>,
        media_units: Option<Decimal>,
    ) {
        if let Some(value) = input_tokens {
            self.span.record("olp.usage.input_tokens", value);
        }
        if let Some(value) = output_tokens {
            self.span.record("olp.usage.output_tokens", value);
        }
        if let Some(value) = cached_input_tokens {
            self.span.record("olp.usage.cached_input_tokens", value);
        }
        if let Some(value) = media_units {
            self.span
                .record("olp.usage.media_units", field::display(value));
        }
    }

    pub(in crate::inference) fn finish(
        &mut self,
        outcome: &str,
        upstream_status_class: Option<&str>,
    ) {
        self.span.record("olp.outcome_class", outcome);
        if let Some(status_class) = upstream_status_class {
            self.span.record("olp.upstream_status_class", status_class);
        }
        self.completed = true;
    }
}

impl Drop for AttemptTrace {
    fn drop(&mut self) {
        if !self.completed {
            self.finish("cancelled", None);
        }
    }
}

fn status_class(status: Option<u16>) -> Option<&'static str> {
    status.map(|code| match code {
        100..=199 => "1xx",
        200..=299 => "2xx",
        300..=399 => "3xx",
        400..=499 => "4xx",
        500..=599 => "5xx",
        _ => "invalid",
    })
}

#[cfg(test)]
mod tests {
    use std::{
        collections::BTreeSet,
        sync::{Arc, Mutex},
    };

    use tracing::{Event, Metadata, Subscriber, field::Visit, span};

    use super::{ATTEMPT_ATTRIBUTE_KEYS, REQUEST_ATTRIBUTE_KEYS, RequestTrace};
    use crate::domain::{
        ids::{DurationMs, ProviderId, RouteId, RuntimeGenerationId, TargetId},
        routing::{provider::ProviderKind, selection::AttemptPlan},
    };

    #[derive(Default)]
    struct Capture {
        span_name: String,
        target: String,
        fields: BTreeSet<String>,
        events: usize,
        links: usize,
        values: Vec<String>,
        recorded_fields: Vec<String>,
    }

    #[derive(Clone, Default)]
    struct CaptureSubscriber(Arc<Mutex<Capture>>);

    impl Subscriber for CaptureSubscriber {
        fn enabled(&self, _: &Metadata<'_>) -> bool {
            true
        }

        fn new_span(&self, attributes: &span::Attributes<'_>) -> span::Id {
            let mut capture = self.0.lock().unwrap();
            capture.span_name = attributes.metadata().name().to_owned();
            capture.target = attributes.metadata().target().to_owned();
            capture.fields.extend(
                attributes
                    .metadata()
                    .fields()
                    .iter()
                    .map(|field| field.name().to_owned()),
            );
            attributes.record(&mut FieldVisitor(&mut capture));
            span::Id::from_u64(1)
        }

        fn record(&self, _: &span::Id, values: &span::Record<'_>) {
            values.record(&mut FieldVisitor(&mut self.0.lock().unwrap()));
        }

        fn record_follows_from(&self, _: &span::Id, _: &span::Id) {
            self.0.lock().unwrap().links += 1;
        }

        fn event(&self, _: &Event<'_>) {
            self.0.lock().unwrap().events += 1;
        }

        fn enter(&self, _: &span::Id) {}

        fn exit(&self, _: &span::Id) {}
    }

    struct FieldVisitor<'a>(&'a mut Capture);

    impl Visit for FieldVisitor<'_> {
        fn record_debug(&mut self, field: &tracing::field::Field, _: &dyn std::fmt::Debug) {
            self.0.fields.insert(field.name().to_owned());
            self.0.recorded_fields.push(field.name().to_owned());
        }

        fn record_str(&mut self, field: &tracing::field::Field, value: &str) {
            self.0.fields.insert(field.name().to_owned());
            self.0.values.push(value.to_owned());
            self.0.recorded_fields.push(field.name().to_owned());
        }
    }

    fn attempt_plan() -> AttemptPlan {
        AttemptPlan {
            generation_id: RuntimeGenerationId::new(),
            route_id: RouteId::new(),
            target_id: TargetId::new(),
            routing_id: TargetId::new(),
            provider_id: ProviderId::new(),
            provider_revision_id: Some(uuid::Uuid::now_v7()),
            provider_kind: ProviderKind::OpenAi,
            upstream_model: "model-under-test".to_owned(),
            timeout: DurationMs::new(1_000),
            priority: 0,
        }
    }

    #[test]
    fn trace_attribute_allowlists_are_exact_and_disjoint() {
        assert_eq!(REQUEST_ATTRIBUTE_KEYS.len(), 13);
        assert_eq!(ATTEMPT_ATTRIBUTE_KEYS.len(), 10);
        for key in REQUEST_ATTRIBUTE_KEYS {
            assert!(key.starts_with("olp."));
            assert!(!ATTEMPT_ATTRIBUTE_KEYS.contains(key));
        }
        for key in ATTEMPT_ATTRIBUTE_KEYS {
            assert!(key.starts_with("olp."));
        }
    }

    #[test]
    fn attempt_callsite_emits_only_allowlisted_attributes_and_no_content_events() {
        let subscriber = CaptureSubscriber::default();
        let captured = Arc::clone(&subscriber.0);
        tracing::subscriber::with_default(subscriber, || {
            let request = RequestTrace::new(tracing::Span::none(), false);
            let mut attempt = request.attempt(&attempt_plan());
            attempt.record_usage(Some(11), Some(7), Some(3), None);
            attempt.finish("success", Some("2xx"));
        });

        let capture = captured.lock().unwrap();
        assert_eq!(capture.span_name, "attempt");
        assert_eq!(capture.target, super::OTEL_TARGET);
        let ordinary_fields = capture
            .fields
            .iter()
            .filter(|field| !field.starts_with("otel."))
            .map(String::as_str)
            .collect::<BTreeSet<_>>();
        assert_eq!(
            ordinary_fields,
            ATTEMPT_ATTRIBUTE_KEYS.iter().copied().collect()
        );
        assert_eq!(capture.events, 0);
        assert_eq!(capture.links, 0);
    }

    #[test]
    fn dropping_an_active_attempt_records_cancellation() {
        let subscriber = CaptureSubscriber::default();
        let captured = Arc::clone(&subscriber.0);
        tracing::subscriber::with_default(subscriber, || {
            let request = RequestTrace::new(tracing::Span::none(), false);
            drop(request.attempt(&attempt_plan()));
        });

        let capture = captured.lock().unwrap();
        assert!(capture.values.iter().any(|value| value == "cancelled"));
        assert_eq!(capture.events, 0);
    }

    #[test]
    fn attempts_only_trace_does_not_rewrite_request_fields() {
        use crate::domain::canonical::identity::{OperationKind, Surface};

        let subscriber = CaptureSubscriber::default();
        let captured = Arc::clone(&subscriber.0);
        tracing::subscriber::with_default(subscriber, || {
            let span = tracing::info_span!(
                "request",
                "olp.surface" = tracing::field::Empty,
                "olp.operation" = tracing::field::Empty,
                "olp.route_slug" = tracing::field::Empty,
                "olp.key_id" = tracing::field::Empty,
                "olp.generation" = tracing::field::Empty,
                "olp.status" = tracing::field::Empty,
                "olp.error_class" = tracing::field::Empty,
                "olp.attempt_count" = tracing::field::Empty,
                "olp.time_to_first_byte_ms" = tracing::field::Empty,
                "olp.total_duration_ms" = tracing::field::Empty,
                "olp.cancelled" = tracing::field::Empty,
            );
            let request = RequestTrace::new(span, true).attempts_only();
            request.record_inference_context(
                Surface::OpenAi,
                OperationKind::VideoGet,
                &crate::domain::ids::RouteSlug::parse("internal-route").unwrap(),
                uuid::Uuid::now_v7(),
                uuid::Uuid::now_v7(),
            );
            request.record_terminal(Some(502), Some("suppressed_error"), 3, Some(1), 2);
            let mut attempt = request.attempt(&attempt_plan());
            attempt.finish("success", Some("2xx"));
        });

        let capture = captured.lock().unwrap();
        assert!(
            capture
                .recorded_fields
                .iter()
                .all(|field| !REQUEST_ATTRIBUTE_KEYS.contains(&field.as_str()))
        );
        assert!(capture.values.iter().any(|value| value == "success"));
    }
}
