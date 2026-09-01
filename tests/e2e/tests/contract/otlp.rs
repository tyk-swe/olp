use std::{
    collections::{BTreeSet, VecDeque},
    sync::{Arc, Mutex},
    time::Duration,
};

use axum::{
    Router,
    body::Bytes,
    extract::{DefaultBodyLimit, State},
    http::{HeaderMap, StatusCode, header},
    response::{IntoResponse, Response},
    routing::post,
};
use opentelemetry_proto::tonic::{
    collector::trace::v1::ExportTraceServiceRequest,
    common::v1::{AnyValue, KeyValue, any_value},
    trace::v1::Span,
};
use prost::Message as _;
use rand::RngCore as _;
use tokio::{sync::Notify, task::JoinHandle};

const MAX_EXPORT_BYTES: usize = 1 << 20;
const MAX_COLLECTED_SPANS: usize = 8_192;

#[derive(Clone)]
struct ReceiverState {
    spans: Arc<Mutex<VecDeque<CollectedSpan>>>,
    changed: Arc<Notify>,
}

impl Default for ReceiverState {
    fn default() -> Self {
        Self {
            spans: Arc::new(Mutex::new(VecDeque::new())),
            changed: Arc::new(Notify::new()),
        }
    }
}

pub(crate) struct OtlpReceiver {
    endpoint: String,
    state: ReceiverState,
    task: JoinHandle<()>,
}

#[derive(Clone)]
pub(crate) struct CollectedSpan {
    pub(crate) span: Span,
    resource_attributes: Vec<KeyValue>,
    scope_name: String,
    scope_version: String,
    scope_attributes: Vec<KeyValue>,
}

pub(crate) struct InboundTrace {
    pub(crate) trace_id: [u8; 16],
    pub(crate) parent_span_id: [u8; 8],
    pub(crate) header: String,
}

impl OtlpReceiver {
    pub(crate) async fn spawn() -> Result<Self, String> {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .map_err(|error| format!("failed to bind OTLP receiver: {error}"))?;
        let address = listener
            .local_addr()
            .map_err(|error| format!("failed to read OTLP receiver address: {error}"))?;
        let state = ReceiverState::default();
        let app = Router::new()
            .route("/v1/traces", post(receive_traces))
            .layer(DefaultBodyLimit::max(MAX_EXPORT_BYTES))
            .with_state(state.clone());
        let task = tokio::spawn(async move {
            let _ = axum::serve(listener, app).await;
        });
        Ok(Self {
            endpoint: format!("http://{address}/v1/traces"),
            state,
            task,
        })
    }

    pub(crate) fn endpoint(&self) -> &str {
        &self.endpoint
    }

    pub(crate) async fn await_trace(
        &self,
        trace_id: &[u8; 16],
        expected_spans: usize,
        timeout: Duration,
    ) -> Result<Vec<CollectedSpan>, String> {
        let deadline = tokio::time::Instant::now() + timeout;
        loop {
            let changed = self.state.changed.notified();
            tokio::pin!(changed);
            changed.as_mut().enable();
            let spans = self.trace(trace_id);
            if spans.len() >= expected_spans {
                return Ok(spans);
            }
            if tokio::time::timeout_at(deadline, changed).await.is_err() {
                return Err(format!(
                    "OTLP receiver saw {} of {expected_spans} expected spans for trace {}: {:?}",
                    spans.len(),
                    hex_id(trace_id),
                    spans
                        .iter()
                        .map(|span| {
                            format!(
                                "{}/{:?}/{:?}",
                                span.span.name,
                                span.string_attribute("olp.provider_kind"),
                                span.string_attribute("olp.outcome_class")
                            )
                        })
                        .collect::<Vec<_>>()
                ));
            }
        }
    }

    fn trace(&self, trace_id: &[u8; 16]) -> Vec<CollectedSpan> {
        self.state
            .spans
            .lock()
            .unwrap()
            .iter()
            .filter(|span| span.span.trace_id.as_slice() == trace_id)
            .cloned()
            .collect()
    }
}

impl Drop for OtlpReceiver {
    fn drop(&mut self) {
        self.task.abort();
    }
}

impl ReceiverState {
    fn record(&self, export: ExportTraceServiceRequest) {
        let mut collected = self.spans.lock().unwrap();
        for resource_spans in export.resource_spans {
            let resource_attributes = resource_spans
                .resource
                .map(|resource| resource.attributes)
                .unwrap_or_default();
            for scope_spans in resource_spans.scope_spans {
                let (scope_name, scope_version, scope_attributes) = scope_spans
                    .scope
                    .map(|scope| (scope.name, scope.version, scope.attributes))
                    .unwrap_or_default();
                for span in scope_spans.spans {
                    if collected.len() == MAX_COLLECTED_SPANS {
                        collected.pop_front();
                    }
                    collected.push_back(CollectedSpan {
                        span,
                        resource_attributes: resource_attributes.clone(),
                        scope_name: scope_name.clone(),
                        scope_version: scope_version.clone(),
                        scope_attributes: scope_attributes.clone(),
                    });
                }
            }
        }
        drop(collected);
        self.changed.notify_waiters();
    }
}

impl CollectedSpan {
    pub(crate) fn attribute_keys(&self) -> BTreeSet<&str> {
        self.span
            .attributes
            .iter()
            .map(|attribute| attribute.key.as_str())
            .collect()
    }

    pub(crate) fn resource_attribute_keys(&self) -> BTreeSet<&str> {
        self.resource_attributes
            .iter()
            .map(|attribute| attribute.key.as_str())
            .collect()
    }

    pub(crate) fn string_attribute(&self, key: &str) -> Option<&str> {
        string_attribute(&self.span.attributes, key)
    }

    pub(crate) fn integer_attribute(&self, key: &str) -> Option<i64> {
        self.span
            .attributes
            .iter()
            .find(|attribute| attribute.key == key)
            .and_then(|attribute| attribute.value.as_ref())
            .and_then(|value| match value.value.as_ref()? {
                any_value::Value::IntValue(value) => Some(*value),
                any_value::Value::StringValue(value) => value.parse().ok(),
                _ => None,
            })
    }

    pub(crate) fn resource_attribute(&self, key: &str) -> Option<&str> {
        string_attribute(&self.resource_attributes, key)
    }

    pub(crate) fn contains_any_text(&self, needles: &[&str]) -> bool {
        contains(&self.span.name, needles)
            || contains(&self.span.trace_state, needles)
            || contains(&self.scope_name, needles)
            || contains(&self.scope_version, needles)
            || key_values_contain(&self.resource_attributes, needles)
            || key_values_contain(&self.scope_attributes, needles)
            || key_values_contain(&self.span.attributes, needles)
            || self.span.events.iter().any(|event| {
                contains(&event.name, needles) || key_values_contain(&event.attributes, needles)
            })
            || self.span.links.iter().any(|link| {
                contains(&link.trace_state, needles)
                    || key_values_contain(&link.attributes, needles)
            })
            || self
                .span
                .status
                .as_ref()
                .is_some_and(|status| contains(&status.message, needles))
    }
}

pub(crate) fn inbound_trace() -> InboundTrace {
    let mut trace_id = [0_u8; 16];
    let mut parent_span_id = [0_u8; 8];
    rand::rng().fill_bytes(&mut trace_id);
    rand::rng().fill_bytes(&mut parent_span_id);
    trace_id[0] |= 1;
    parent_span_id[0] |= 1;
    InboundTrace {
        header: format!("00-{}-{}-01", hex_id(&trace_id), hex_id(&parent_span_id)),
        trace_id,
        parent_span_id,
    }
}

pub(crate) fn hex_id(bytes: &[u8]) -> String {
    bytes.iter().map(|byte| format!("{byte:02x}")).collect()
}

async fn receive_traces(
    State(state): State<ReceiverState>,
    headers: HeaderMap,
    body: Bytes,
) -> Response {
    let protobuf = headers
        .get(header::CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .is_some_and(|value| value.starts_with("application/x-protobuf"));
    if !protobuf {
        return StatusCode::UNSUPPORTED_MEDIA_TYPE.into_response();
    }
    let Ok(export) = ExportTraceServiceRequest::decode(body) else {
        return StatusCode::BAD_REQUEST.into_response();
    };
    state.record(export);
    (
        [(header::CONTENT_TYPE, "application/x-protobuf")],
        Vec::<u8>::new(),
    )
        .into_response()
}

fn string_attribute<'a>(attributes: &'a [KeyValue], key: &str) -> Option<&'a str> {
    attributes
        .iter()
        .find(|attribute| attribute.key == key)
        .and_then(|attribute| attribute.value.as_ref())
        .and_then(|value| match value.value.as_ref()? {
            any_value::Value::StringValue(value) => Some(value.as_str()),
            _ => None,
        })
}

fn key_values_contain(attributes: &[KeyValue], needles: &[&str]) -> bool {
    attributes.iter().any(|attribute| {
        contains(&attribute.key, needles)
            || attribute
                .value
                .as_ref()
                .is_some_and(|value| any_value_contains(value, needles))
    })
}

fn any_value_contains(value: &AnyValue, needles: &[&str]) -> bool {
    match value.value.as_ref() {
        Some(any_value::Value::StringValue(value)) => contains(value, needles),
        Some(any_value::Value::ArrayValue(value)) => value
            .values
            .iter()
            .any(|value| any_value_contains(value, needles)),
        Some(any_value::Value::KvlistValue(value)) => key_values_contain(&value.values, needles),
        Some(any_value::Value::BytesValue(value)) => {
            contains(&String::from_utf8_lossy(value), needles)
        }
        Some(
            any_value::Value::BoolValue(_)
            | any_value::Value::IntValue(_)
            | any_value::Value::DoubleValue(_)
            | any_value::Value::StringValueStrindex(_),
        )
        | None => false,
    }
}

fn contains(value: &str, needles: &[&str]) -> bool {
    needles.iter().any(|needle| value.contains(needle))
}
