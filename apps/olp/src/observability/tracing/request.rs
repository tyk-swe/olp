use std::{
    pin::Pin,
    task::{Context, Poll},
    time::Instant,
};

use axum::{
    body::{Body, Bytes},
    http::{HeaderMap, Method, Request, StatusCode},
    middleware,
    response::Response,
};
use http_body::{Frame, SizeHint};
use opentelemetry::{
    Context as OtelContext,
    propagation::TextMapPropagator as _,
    trace::{SpanContext, TraceContextExt as _, TraceState},
};
use opentelemetry_http::HeaderExtractor;
use opentelemetry_sdk::propagation::TraceContextPropagator;
use tracing::{Instrument as _, Span, field};
use tracing_opentelemetry::OpenTelemetrySpanExt as _;

use crate::gateway::endpoint_policy::classification::InferenceEndpoint;

use super::{OTEL_TARGET, RequestConfig};

const CLIENT_CANCELLED: &str = "client_cancelled";
const TRACEPARENT: &str = "traceparent";

struct TracedBody {
    inner: Body,
    span: Span,
    started: Instant,
    status: StatusCode,
    first_byte_recorded: bool,
    finished: bool,
}

struct RequestFutureGuard {
    span: Span,
    started: Instant,
    armed: bool,
}

impl RequestFutureGuard {
    fn disarm(mut self) -> Instant {
        self.armed = false;
        self.started
    }
}

impl Drop for RequestFutureGuard {
    fn drop(&mut self) {
        if !self.armed {
            return;
        }
        self.span.record("olp.error_class", CLIENT_CANCELLED);
        self.span.record("olp.cancelled", true);
        self.span
            .record("olp.total_duration_ms", elapsed_milliseconds(self.started));
    }
}

impl TracedBody {
    fn new(inner: Body, span: Span, started: Instant, method: &Method, status: StatusCode) -> Self {
        let bodyless = method == Method::HEAD
            || (method == Method::CONNECT && status.is_success())
            || status.is_informational()
            || matches!(
                status,
                StatusCode::NO_CONTENT | StatusCode::RESET_CONTENT | StatusCode::NOT_MODIFIED
            )
            || http_body::Body::is_end_stream(&inner)
            || http_body::Body::size_hint(&inner).exact() == Some(0);
        let mut traced = Self {
            inner,
            span,
            started,
            status,
            first_byte_recorded: false,
            finished: false,
        };
        if bodyless {
            traced.finish(None);
        }
        traced
    }

    fn record_first_byte(&mut self) {
        if self.first_byte_recorded {
            return;
        }
        self.span.record(
            "olp.time_to_first_byte_ms",
            elapsed_milliseconds(self.started),
        );
        self.first_byte_recorded = true;
    }

    fn finish(&mut self, error_class: Option<&'static str>) {
        if self.finished {
            return;
        }
        self.record_first_byte();
        self.span
            .record("olp.status", u64::from(self.status.as_u16()));
        self.span
            .record("olp.total_duration_ms", elapsed_milliseconds(self.started));
        if let Some(error_class) = error_class {
            self.span.record("olp.error_class", error_class);
            if error_class == CLIENT_CANCELLED {
                self.span.record("olp.cancelled", true);
            }
        }
        self.finished = true;
    }
}

impl http_body::Body for TracedBody {
    type Data = Bytes;
    type Error = axum::Error;

    fn poll_frame(
        self: Pin<&mut Self>,
        context: &mut Context<'_>,
    ) -> Poll<Option<Result<Frame<Self::Data>, Self::Error>>> {
        let this = self.get_mut();
        let polled = {
            let _entered = this.span.enter();
            Pin::new(&mut this.inner).poll_frame(context)
        };
        match polled {
            Poll::Ready(None) => {
                this.finish(None);
                Poll::Ready(None)
            }
            Poll::Ready(Some(Ok(frame))) => {
                if frame.data_ref().is_some() {
                    this.record_first_byte();
                }
                Poll::Ready(Some(Ok(frame)))
            }
            Poll::Ready(Some(Err(error))) => {
                this.finish(Some("response_body"));
                Poll::Ready(Some(Err(error)))
            }
            Poll::Pending => Poll::Pending,
        }
    }

    fn is_end_stream(&self) -> bool {
        self.finished && self.inner.is_end_stream()
    }

    fn size_hint(&self) -> SizeHint {
        self.inner.size_hint()
    }
}

impl Drop for TracedBody {
    fn drop(&mut self) {
        self.finish(Some(CLIENT_CANCELLED));
    }
}

pub(crate) async fn trace_admitted_request(
    config: RequestConfig,
    endpoint: Option<InferenceEndpoint>,
    mut request: Request<Body>,
    next: middleware::Next,
) -> Response {
    let method = request.method().clone();
    let span = request_span(config, endpoint, &request);
    if config.accept_inbound && request.headers().contains_key(TRACEPARENT) {
        let parent = extract_parent(request.headers());
        let _ = span.set_parent(parent);
    }
    request
        .extensions_mut()
        .insert(olp_engine::inference::tracing::RequestTrace::new(
            span.clone(),
            config.propagate_upstream,
        ));
    let guard = RequestFutureGuard {
        span: span.clone(),
        started: Instant::now(),
        armed: true,
    };
    let response = next.run(request).instrument(span.clone()).await;
    let started = guard.disarm();
    let status = response.status();
    let (parts, body) = response.into_parts();
    Response::from_parts(
        parts,
        Body::new(TracedBody::new(body, span, started, &method, status)),
    )
}

fn request_span(
    config: RequestConfig,
    endpoint: Option<InferenceEndpoint>,
    request: &Request<Body>,
) -> Span {
    let span = tracing::info_span!(
        target: OTEL_TARGET,
        parent: None,
        "request",
        otel.name = "request",
        otel.kind = "server",
        "olp.request_id" = field::Empty,
        "olp.surface" = field::Empty,
        "olp.operation" = field::Empty,
        "olp.route_slug" = field::Empty,
        "olp.key_id" = field::Empty,
        "olp.installation_id" = field::Empty,
        "olp.generation" = field::Empty,
        "olp.status" = field::Empty,
        "olp.error_class" = field::Empty,
        "olp.attempt_count" = field::Empty,
        "olp.time_to_first_byte_ms" = field::Empty,
        "olp.total_duration_ms" = field::Empty,
        "olp.cancelled" = field::Empty,
    );
    record_admission_attributes(&span, config, endpoint, request.headers());
    span
}

fn record_admission_attributes(
    span: &Span,
    config: RequestConfig,
    endpoint: Option<InferenceEndpoint>,
    headers: &HeaderMap,
) {
    span.record(
        "olp.installation_id",
        field::display(config.installation_id),
    );
    if let Some(request_id) = trace_request_id(headers) {
        span.record("olp.request_id", field::display(request_id));
    }
    if let Some(endpoint) = endpoint {
        span.record("olp.surface", field::display(endpoint.surface()));
        if let Some(metadata) = endpoint.metadata() {
            span.record("olp.operation", field::display(metadata.operation));
        }
    } else {
        span.record("olp.surface", "management");
        span.record("olp.operation", "management");
    }
}

fn trace_request_id(headers: &HeaderMap) -> Option<uuid::Uuid> {
    let value = headers.get("x-request-id")?.to_str().ok()?;
    let request_id = value.parse::<uuid::Uuid>().ok()?;
    let mut canonical = [0_u8; uuid::fmt::Hyphenated::LENGTH];
    (request_id.hyphenated().encode_lower(&mut canonical) == value).then_some(request_id)
}

fn extract_parent(headers: &HeaderMap) -> OtelContext {
    let extracted = TraceContextPropagator::new().extract(&HeaderExtractor(headers));
    let extracted_span = extracted.span();
    let parent = extracted_span.span_context();
    OtelContext::new().with_remote_span_context(SpanContext::new(
        parent.trace_id(),
        parent.span_id(),
        parent.trace_flags(),
        parent.is_remote(),
        TraceState::default(),
    ))
}

fn elapsed_milliseconds(started: Instant) -> u64 {
    u64::try_from(started.elapsed().as_millis()).unwrap_or(u64::MAX)
}

#[cfg(test)]
mod tests;
