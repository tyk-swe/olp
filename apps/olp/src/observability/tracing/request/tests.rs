use std::sync::{Arc, Mutex};

use std::time::Instant;

use axum::{
    body::Body,
    http::{Method, Request, StatusCode},
};
use opentelemetry::trace::TraceContextExt as _;
use tracing::{Subscriber, field::Visit};
use tracing_subscriber::{Layer, layer::SubscriberExt as _};

use super::{RequestConfig, TracedBody, extract_parent, request_span, trace_request_id};

#[derive(Clone, Default)]
struct Capture {
    declared: Arc<Mutex<Vec<String>>>,
    values: Arc<Mutex<Vec<String>>>,
}

struct ValueVisitor<'a>(&'a Mutex<Vec<String>>);

impl Visit for ValueVisitor<'_> {
    fn record_debug(&mut self, field: &tracing::field::Field, value: &dyn std::fmt::Debug) {
        self.0
            .lock()
            .unwrap()
            .push(format!("{}={value:?}", field.name()));
    }
}

impl<S: Subscriber> Layer<S> for Capture {
    fn on_new_span(
        &self,
        attributes: &tracing::span::Attributes<'_>,
        _id: &tracing::span::Id,
        _context: tracing_subscriber::layer::Context<'_, S>,
    ) {
        if attributes.metadata().name() != "request" {
            return;
        }
        self.declared.lock().unwrap().extend(
            attributes
                .metadata()
                .fields()
                .iter()
                .map(|field| field.name().to_owned()),
        );
        attributes.record(&mut ValueVisitor(&self.values));
    }

    fn on_record(
        &self,
        _span: &tracing::span::Id,
        values: &tracing::span::Record<'_>,
        _context: tracing_subscriber::layer::Context<'_, S>,
    ) {
        values.record(&mut ValueVisitor(&self.values));
    }
}

fn request_config() -> RequestConfig {
    RequestConfig {
        installation_id: uuid::Uuid::from_u128(1),
        propagate_upstream: true,
        accept_inbound: true,
    }
}

#[test]
fn request_span_callsite_is_allowlisted_and_omits_untrusted_request_id_content() {
    let capture = Capture::default();
    let subscriber = tracing_subscriber::registry().with(capture.clone());
    tracing::subscriber::with_default(subscriber, || {
        let request = Request::builder()
            .header("x-request-id", "secret prompt in correlation header")
            .body(Body::empty())
            .unwrap();
        drop(request_span(request_config(), None, &request));
    });

    let mut declared = capture.declared.lock().unwrap().clone();
    declared.sort();
    let mut expected = olp_engine::inference::tracing::REQUEST_ATTRIBUTE_KEYS
        .iter()
        .map(|key| (*key).to_owned())
        .chain(["otel.kind".to_owned(), "otel.name".to_owned()])
        .collect::<Vec<_>>();
    expected.sort();
    assert_eq!(declared, expected);
    assert!(
        capture
            .values
            .lock()
            .unwrap()
            .iter()
            .all(|value| !value.contains("secret prompt"))
    );
}

#[test]
fn bodyless_responses_finish_without_recording_cancellation() {
    let capture = Capture::default();
    let subscriber = tracing_subscriber::registry().with(capture.clone());
    tracing::subscriber::with_default(subscriber, || {
        for (method, status, body) in [
            (Method::GET, StatusCode::OK, Body::empty()),
            (Method::HEAD, StatusCode::OK, Body::from("ignored")),
            (Method::GET, StatusCode::NO_CONTENT, Body::from("ignored")),
            (Method::GET, StatusCode::NOT_MODIFIED, Body::from("ignored")),
        ] {
            let request = Request::new(Body::empty());
            let span = request_span(request_config(), None, &request);
            drop(TracedBody::new(body, span, Instant::now(), &method, status));
        }
    });

    let values = capture.values.lock().unwrap();
    assert!(
        values
            .iter()
            .all(|value| !value.contains("client_cancelled") && value != "olp.cancelled=true")
    );
    assert!(values.iter().any(|value| value == "olp.status=204"));
    assert!(values.iter().any(|value| value == "olp.status=304"));
}

#[test]
fn dropping_an_unread_nonempty_response_records_cancellation() {
    let capture = Capture::default();
    let subscriber = tracing_subscriber::registry().with(capture.clone());
    tracing::subscriber::with_default(subscriber, || {
        let request = Request::new(Body::empty());
        let span = request_span(request_config(), None, &request);
        drop(TracedBody::new(
            Body::from("unread"),
            span,
            Instant::now(),
            &Method::GET,
            StatusCode::OK,
        ));
    });

    let values = capture.values.lock().unwrap();
    assert!(
        values
            .iter()
            .any(|value| value.contains("client_cancelled"))
    );
    assert!(values.iter().any(|value| value == "olp.cancelled=true"));
}

#[test]
fn trace_request_id_accepts_only_canonical_uuid_values() {
    let valid = uuid::Uuid::from_u128(0xabcdefab_cdef_abcd_efab_cdefabcdefab);
    let mut valid_headers = axum::http::HeaderMap::new();
    valid_headers.insert("x-request-id", valid.to_string().parse().unwrap());
    let mut untrusted_headers = axum::http::HeaderMap::new();
    untrusted_headers.insert("x-request-id", "secret prompt".parse().unwrap());

    assert_eq!(trace_request_id(&valid_headers), Some(valid));
    assert_eq!(trace_request_id(&untrusted_headers), None);

    for noncanonical in [
        valid.simple().to_string(),
        valid.braced().to_string(),
        valid.urn().to_string(),
        valid.to_string().to_uppercase(),
    ] {
        let mut headers = axum::http::HeaderMap::new();
        headers.insert("x-request-id", noncanonical.parse().unwrap());
        assert_eq!(trace_request_id(&headers), None);
    }
}

#[test]
fn inbound_parent_discards_caller_tracestate() {
    let mut headers = axum::http::HeaderMap::new();
    headers.insert(
        "traceparent",
        "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
            .parse()
            .unwrap(),
    );
    headers.insert("tracestate", "vendor=secret-trace-prompt".parse().unwrap());

    let parent = extract_parent(&headers);
    let span = parent.span();
    let context = span.span_context();

    assert_eq!(
        context.trace_id().to_string(),
        "4bf92f3577b34da6a3ce929d0e0e4736"
    );
    assert_eq!(context.span_id().to_string(), "00f067aa0ba902b7");
    assert!(context.is_remote());
    assert_eq!(context.trace_state().header(), "");
}
