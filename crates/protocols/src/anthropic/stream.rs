use std::collections::{BTreeMap, BTreeSet};

use olp_domain::{
    CanonicalError, CanonicalEvent, CanonicalEventKind, ErrorClass, MessageRole, SourceExtensions,
    Surface, Usage as CanonicalUsage,
};
use serde::Deserialize;
use serde_json::Value;
use thiserror::Error;

use super::{
    ContentBlock, MessagesResponse, Role,
    translate::{anthropic_finish_reason, collect_extra},
};
use crate::sse::{
    DEFAULT_MAX_EVENT_BYTES, SseDecodeError, SseDecoder, SseFrame, raw_sse_frame_event,
};

pub struct AnthropicMessagesStreamDecoder {
    sse: SseDecoder,
    sequence: u64,
    response_started: bool,
    message_started: bool,
    finished: bool,
    done: bool,
    input_tokens: u64,
    output_tokens: u64,
    cache_creation_input_tokens: u64,
    cached_input_tokens: Option<u64>,
    blocks: BTreeMap<u32, BlockState>,
    next_tool_index: u32,
    preserve_raw_frames: bool,
}

impl std::fmt::Debug for AnthropicMessagesStreamDecoder {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("AnthropicMessagesStreamDecoder")
            .field("next_sequence", &self.sequence)
            .field("response_started", &self.response_started)
            .field("open_block_count", &self.blocks.len())
            .field("finished", &self.finished)
            .field("done", &self.done)
            .finish_non_exhaustive()
    }
}

impl Default for AnthropicMessagesStreamDecoder {
    fn default() -> Self {
        Self::new()
    }
}

impl AnthropicMessagesStreamDecoder {
    #[must_use]
    pub fn new() -> Self {
        Self::with_max_event_bytes(DEFAULT_MAX_EVENT_BYTES)
    }

    #[must_use]
    pub fn with_max_event_bytes(max_event_bytes: usize) -> Self {
        Self::with_max_event_bytes_and_raw_passthrough(max_event_bytes, false)
    }

    #[must_use]
    pub fn with_max_event_bytes_and_raw_passthrough(
        max_event_bytes: usize,
        preserve_raw_frames: bool,
    ) -> Self {
        Self {
            sse: SseDecoder::new(max_event_bytes),
            sequence: 0,
            response_started: false,
            message_started: false,
            finished: false,
            done: false,
            input_tokens: 0,
            output_tokens: 0,
            cache_creation_input_tokens: 0,
            cached_input_tokens: None,
            blocks: BTreeMap::new(),
            next_tool_index: 0,
            preserve_raw_frames,
        }
    }

    pub fn push(&mut self, bytes: &[u8]) -> Result<Vec<CanonicalEvent>, StreamError> {
        let frames = self.sse.push(bytes)?;
        self.decode_frames(frames)
    }

    pub fn finish(&mut self) -> Result<Vec<CanonicalEvent>, StreamError> {
        let frames = self.sse.finish()?;
        let events = self.decode_frames(frames)?;
        if !self.done {
            return Err(StreamError::UnexpectedEof);
        }
        Ok(events)
    }

    #[must_use]
    pub const fn is_done(&self) -> bool {
        self.done
    }

    fn decode_frames(&mut self, frames: Vec<SseFrame>) -> Result<Vec<CanonicalEvent>, StreamError> {
        let mut events = Vec::new();
        for frame in frames {
            if self.done {
                return Err(StreamError::DataAfterDone);
            }
            let raw_frame = self.preserve_raw_frames.then(|| frame.clone());
            let event_start = events.len();
            let sequence_start = self.sequence;
            let value: Value = serde_json::from_str(&frame.data)?;
            let data_type = value
                .get("type")
                .and_then(Value::as_str)
                .ok_or(StreamError::MissingEventType)?
                .to_owned();
            let event_type = frame.event.as_deref().unwrap_or(&data_type);
            if event_type != data_type {
                return Err(StreamError::EventTypeMismatch {
                    event: event_type.to_owned(),
                    data: data_type,
                });
            }
            let mut payload = value;
            if is_known_event(&data_type)
                && let Some(object) = payload.as_object_mut()
            {
                object.remove("type");
            }
            match event_type {
                "message_start" => self.message_start(payload, &mut events)?,
                "content_block_start" => self.content_block_start(payload, &mut events)?,
                "content_block_delta" => self.content_block_delta(payload, &mut events)?,
                "content_block_stop" => self.content_block_stop(payload, &mut events)?,
                "message_delta" => self.message_delta(payload, &mut events)?,
                "message_stop" => self.message_stop(payload, &mut events)?,
                "ping" => self.ping(payload, &mut events)?,
                "error" => self.error(payload, &mut events)?,
                other => self.unknown_event(other, payload, &mut events),
            }
            if let Some(raw_frame) = raw_frame {
                let semantic_events = events.len().saturating_sub(event_start);
                for event in &mut events[event_start..] {
                    event.sequence = event.sequence.saturating_add(1);
                }
                events.insert(
                    event_start,
                    raw_sse_frame_event(
                        sequence_start,
                        Surface::Anthropic,
                        &raw_frame,
                        semantic_events,
                    ),
                );
                self.sequence = self.sequence.saturating_add(1);
            }
        }
        Ok(events)
    }

    fn message_start(
        &mut self,
        value: Value,
        events: &mut Vec<CanonicalEvent>,
    ) -> Result<(), StreamError> {
        if self.response_started {
            return Err(StreamError::DuplicateMessageStart);
        }
        let event: MessageStart = serde_json::from_value(value)?;
        if event.message.kind != "message" {
            return Err(StreamError::UnexpectedMessageType(event.message.kind));
        }
        if event.message.role != Role::Assistant {
            return Err(StreamError::UnexpectedMessageRole);
        }
        if !event.message.content.is_empty()
            || event.message.stop_reason.is_some()
            || event.message.stop_sequence.is_some()
        {
            return Err(StreamError::InvalidMessageStartState);
        }
        self.input_tokens = event.message.usage.input_tokens;
        self.output_tokens = event.message.usage.output_tokens;
        self.cache_creation_input_tokens =
            event.message.usage.cache_creation_input_tokens.unwrap_or(0);
        self.cached_input_tokens = event.message.usage.cache_read_input_tokens;
        self.emit(
            events,
            CanonicalEventKind::ResponseStart {
                response_id: Some(event.message.id),
                provider_model: Some(event.message.model),
            },
        );
        self.emit(
            events,
            CanonicalEventKind::MessageStart {
                output_index: 0,
                role: MessageRole::Assistant,
            },
        );
        self.response_started = true;
        self.message_started = true;

        let mut extensions = BTreeMap::new();
        collect_extra("", &event.extra, &mut extensions);
        collect_extra("/message", &event.message.extra, &mut extensions);
        collect_extra(
            "/message/usage",
            &event.message.usage.extra,
            &mut extensions,
        );
        if let Some(tokens) = event.message.usage.cache_creation_input_tokens {
            extensions.insert(
                "/message/usage/cache_creation_input_tokens".into(),
                Value::from(tokens),
            );
        }
        self.emit_extensions(events, extensions);
        Ok(())
    }

    fn content_block_start(
        &mut self,
        value: Value,
        events: &mut Vec<CanonicalEvent>,
    ) -> Result<(), StreamError> {
        self.require_started()?;
        self.require_not_finished()?;
        let event: ContentBlockStart = serde_json::from_value(value)?;
        if self.blocks.contains_key(&event.index) {
            return Err(StreamError::DuplicateContentBlock(event.index));
        }
        let mut extensions = BTreeMap::new();
        collect_extra("", &event.extra, &mut extensions);
        match event.content_block {
            ContentBlock::Text(block) if block.kind == "text" => {
                collect_extra(
                    &format!("/content/{}", event.index),
                    &block.extra,
                    &mut extensions,
                );
                if !block.text.is_empty() {
                    self.emit(
                        events,
                        CanonicalEventKind::TextDelta {
                            output_index: 0,
                            text: block.text,
                        },
                    );
                }
                self.blocks.insert(event.index, BlockState::Text);
            }
            ContentBlock::ToolUse(block) if block.kind == "tool_use" => {
                let tool_index = self.next_tool_index;
                self.next_tool_index = self
                    .next_tool_index
                    .checked_add(1)
                    .ok_or(StreamError::TooManyToolCalls)?;
                collect_extra(
                    &format!("/content/{}", event.index),
                    &block.extra,
                    &mut extensions,
                );
                let arguments_delta = if block
                    .input
                    .as_object()
                    .is_some_and(serde_json::Map::is_empty)
                {
                    String::new()
                } else {
                    serde_json::to_string(&block.input)?
                };
                self.emit(
                    events,
                    CanonicalEventKind::ToolCallDelta {
                        output_index: 0,
                        tool_index,
                        id: Some(block.id),
                        name: Some(block.name),
                        arguments_delta,
                    },
                );
                self.blocks
                    .insert(event.index, BlockState::Tool(tool_index));
            }
            block => {
                let kind = block.kind().unwrap_or("unknown").to_owned();
                extensions.insert(format!("/content/{}", event.index), block.as_value());
                self.blocks.insert(event.index, BlockState::Extension(kind));
            }
        }
        self.emit_extensions(events, extensions);
        Ok(())
    }

    fn content_block_delta(
        &mut self,
        value: Value,
        events: &mut Vec<CanonicalEvent>,
    ) -> Result<(), StreamError> {
        self.require_started()?;
        self.require_not_finished()?;
        let event: ContentBlockDelta = serde_json::from_value(value)?;
        let block = self
            .blocks
            .get(&event.index)
            .ok_or(StreamError::UnknownContentBlock(event.index))?
            .clone();
        let delta_type = event
            .delta
            .get("type")
            .and_then(Value::as_str)
            .ok_or(StreamError::MissingDeltaType)?;
        let mut extensions = BTreeMap::new();
        collect_extra("", &event.extra, &mut extensions);
        match (delta_type, block) {
            ("text_delta", BlockState::Text) => {
                let text = event
                    .delta
                    .get("text")
                    .and_then(Value::as_str)
                    .ok_or(StreamError::MissingDeltaField("text"))?;
                self.emit(
                    events,
                    CanonicalEventKind::TextDelta {
                        output_index: 0,
                        text: text.to_owned(),
                    },
                );
                collect_unknown_delta_fields(
                    &event.delta,
                    &["type", "text"],
                    &format!("/content/{}/delta", event.index),
                    &mut extensions,
                );
            }
            ("input_json_delta", BlockState::Tool(tool_index)) => {
                let partial_json = event
                    .delta
                    .get("partial_json")
                    .and_then(Value::as_str)
                    .ok_or(StreamError::MissingDeltaField("partial_json"))?;
                self.emit(
                    events,
                    CanonicalEventKind::ToolCallDelta {
                        output_index: 0,
                        tool_index,
                        id: None,
                        name: None,
                        arguments_delta: partial_json.to_owned(),
                    },
                );
                collect_unknown_delta_fields(
                    &event.delta,
                    &["type", "partial_json"],
                    &format!("/content/{}/delta", event.index),
                    &mut extensions,
                );
            }
            (_, BlockState::Extension(kind)) => {
                extensions.insert(
                    format!("/content/{}/delta/{kind}", event.index),
                    event.delta,
                );
            }
            _ => {
                return Err(StreamError::DeltaBlockMismatch {
                    index: event.index,
                    delta: delta_type.to_owned(),
                });
            }
        }
        self.emit_extensions(events, extensions);
        Ok(())
    }

    fn content_block_stop(
        &mut self,
        value: Value,
        events: &mut Vec<CanonicalEvent>,
    ) -> Result<(), StreamError> {
        self.require_not_finished()?;
        let event: ContentBlockStop = serde_json::from_value(value)?;
        if self.blocks.remove(&event.index).is_none() {
            return Err(StreamError::UnknownContentBlock(event.index));
        }
        let mut extensions = BTreeMap::new();
        collect_extra("", &event.extra, &mut extensions);
        self.emit_extensions(events, extensions);
        Ok(())
    }

    fn message_delta(
        &mut self,
        value: Value,
        events: &mut Vec<CanonicalEvent>,
    ) -> Result<(), StreamError> {
        self.require_started()?;
        let event: MessageDelta = serde_json::from_value(value)?;
        if let Some(input_tokens) = event.usage.input_tokens {
            self.input_tokens = input_tokens;
        }
        if let Some(output_tokens) = event.usage.output_tokens {
            self.output_tokens = output_tokens;
        }
        if event.usage.cache_read_input_tokens.is_some() {
            self.cached_input_tokens = event.usage.cache_read_input_tokens;
        }
        if let Some(tokens) = event.usage.cache_creation_input_tokens {
            self.cache_creation_input_tokens = tokens;
        }
        let mut extensions = BTreeMap::new();
        collect_extra("", &event.extra, &mut extensions);
        collect_extra("/delta", &event.delta.extra, &mut extensions);
        collect_extra("/usage", &event.usage.extra, &mut extensions);
        if let Some(stop_sequence) = event.delta.stop_sequence {
            extensions.insert("/delta/stop_sequence".into(), Value::String(stop_sequence));
        }
        if let Some(tokens) = event.usage.cache_creation_input_tokens {
            extensions.insert(
                "/usage/cache_creation_input_tokens".into(),
                Value::from(tokens),
            );
        }
        self.emit_extensions(events, extensions);
        self.emit(
            events,
            CanonicalEventKind::Usage {
                usage: CanonicalUsage {
                    input_tokens: self
                        .input_tokens
                        .saturating_add(self.cache_creation_input_tokens)
                        .saturating_add(self.cached_input_tokens.unwrap_or(0)),
                    output_tokens: self.output_tokens,
                    total_tokens: self
                        .input_tokens
                        .saturating_add(self.cache_creation_input_tokens)
                        .saturating_add(self.cached_input_tokens.unwrap_or(0))
                        .saturating_add(self.output_tokens),
                    cached_input_tokens: self.cached_input_tokens,
                    reasoning_tokens: None,
                },
            },
        );
        if let Some(reason) = event.delta.stop_reason {
            if self.finished {
                return Err(StreamError::DuplicateFinishReason);
            }
            self.emit(
                events,
                CanonicalEventKind::Finish {
                    output_index: 0,
                    reason: anthropic_finish_reason(&reason),
                },
            );
            self.finished = true;
        }
        Ok(())
    }

    fn message_stop(
        &mut self,
        value: Value,
        events: &mut Vec<CanonicalEvent>,
    ) -> Result<(), StreamError> {
        self.require_started()?;
        if !self.finished {
            return Err(StreamError::MessageStoppedWithoutFinishReason);
        }
        if !self.blocks.is_empty() {
            return Err(StreamError::MessageStoppedWithOpenBlocks(
                self.blocks.keys().copied().collect(),
            ));
        }
        let event: SimpleEvent = serde_json::from_value(value)?;
        let mut extensions = BTreeMap::new();
        collect_extra("", &event.extra, &mut extensions);
        self.emit_extensions(events, extensions);
        self.emit(events, CanonicalEventKind::Done);
        self.done = true;
        Ok(())
    }

    fn ping(&mut self, value: Value, events: &mut Vec<CanonicalEvent>) -> Result<(), StreamError> {
        let event: SimpleEvent = serde_json::from_value(value)?;
        let mut extensions = BTreeMap::new();
        collect_extra("", &event.extra, &mut extensions);
        self.emit_extensions(events, extensions);
        Ok(())
    }

    fn error(&mut self, value: Value, events: &mut Vec<CanonicalEvent>) -> Result<(), StreamError> {
        let event: ErrorEvent = serde_json::from_value(value)?;
        let mut extensions = BTreeMap::new();
        collect_extra("", &event.extra, &mut extensions);
        collect_extra("/error", &event.error.extra, &mut extensions);
        self.emit_extensions(events, extensions);
        let (class, retryable) = match event.error.kind.as_str() {
            "authentication_error" => (ErrorClass::Authentication, false),
            "permission_error" => (ErrorClass::Authorization, false),
            "invalid_request_error" => (ErrorClass::InvalidRequest, false),
            "rate_limit_error" | "overloaded_error" => (ErrorClass::RateLimit, true),
            _ => (ErrorClass::Upstream, false),
        };
        self.emit(
            events,
            CanonicalEventKind::Error {
                error: CanonicalError {
                    class,
                    message: event.error.message,
                    provider_code: Some(event.error.kind),
                    retryable,
                },
            },
        );
        self.emit(events, CanonicalEventKind::Done);
        self.done = true;
        Ok(())
    }

    fn unknown_event(&mut self, kind: &str, value: Value, events: &mut Vec<CanonicalEvent>) {
        self.emit_extensions(events, BTreeMap::from([(format!("/events/{kind}"), value)]));
    }

    fn require_started(&self) -> Result<(), StreamError> {
        if self.response_started && self.message_started {
            Ok(())
        } else {
            Err(StreamError::EventBeforeMessageStart)
        }
    }

    fn require_not_finished(&self) -> Result<(), StreamError> {
        if self.finished {
            Err(StreamError::ContentAfterFinish)
        } else {
            Ok(())
        }
    }

    fn emit_extensions(
        &mut self,
        events: &mut Vec<CanonicalEvent>,
        extensions: BTreeMap<String, Value>,
    ) {
        if !extensions.is_empty() {
            self.emit(
                events,
                CanonicalEventKind::SourceExtension {
                    extensions: SourceExtensions::new(Surface::Anthropic, extensions),
                },
            );
        }
    }

    fn emit(&mut self, events: &mut Vec<CanonicalEvent>, kind: CanonicalEventKind) {
        events.push(CanonicalEvent::new(self.sequence, kind));
        self.sequence = self.sequence.saturating_add(1);
    }
}

#[derive(Clone, Debug)]
enum BlockState {
    Text,
    Tool(u32),
    Extension(String),
}

#[derive(Deserialize)]
struct MessageStart {
    message: MessagesResponse,
    #[serde(flatten)]
    extra: BTreeMap<String, Value>,
}

#[derive(Deserialize)]
struct ContentBlockStart {
    index: u32,
    content_block: ContentBlock,
    #[serde(flatten)]
    extra: BTreeMap<String, Value>,
}

#[derive(Deserialize)]
struct ContentBlockDelta {
    index: u32,
    delta: Value,
    #[serde(flatten)]
    extra: BTreeMap<String, Value>,
}

#[derive(Deserialize)]
struct ContentBlockStop {
    index: u32,
    #[serde(flatten)]
    extra: BTreeMap<String, Value>,
}

#[derive(Deserialize)]
struct MessageDelta {
    delta: MessageDeltaFields,
    #[serde(default)]
    usage: StreamUsage,
    #[serde(flatten)]
    extra: BTreeMap<String, Value>,
}

#[derive(Deserialize)]
struct MessageDeltaFields {
    #[serde(default)]
    stop_reason: Option<String>,
    #[serde(default)]
    stop_sequence: Option<String>,
    #[serde(flatten)]
    extra: BTreeMap<String, Value>,
}

#[derive(Default, Deserialize)]
struct StreamUsage {
    #[serde(default)]
    input_tokens: Option<u64>,
    #[serde(default)]
    output_tokens: Option<u64>,
    #[serde(default)]
    cache_creation_input_tokens: Option<u64>,
    #[serde(default)]
    cache_read_input_tokens: Option<u64>,
    #[serde(flatten)]
    extra: BTreeMap<String, Value>,
}

#[derive(Deserialize)]
struct SimpleEvent {
    #[serde(flatten)]
    extra: BTreeMap<String, Value>,
}

#[derive(Deserialize)]
struct ErrorEvent {
    error: WireError,
    #[serde(flatten)]
    extra: BTreeMap<String, Value>,
}

#[derive(Deserialize)]
struct WireError {
    #[serde(rename = "type")]
    kind: String,
    message: String,
    #[serde(flatten)]
    extra: BTreeMap<String, Value>,
}

fn is_known_event(kind: &str) -> bool {
    matches!(
        kind,
        "message_start"
            | "content_block_start"
            | "content_block_delta"
            | "content_block_stop"
            | "message_delta"
            | "message_stop"
            | "ping"
            | "error"
    )
}

fn collect_unknown_delta_fields(
    delta: &Value,
    known: &[&str],
    prefix: &str,
    extensions: &mut BTreeMap<String, Value>,
) {
    let Some(object) = delta.as_object() else {
        return;
    };
    for (key, value) in object {
        if !known.contains(&key.as_str()) {
            let key = key.replace('~', "~0").replace('/', "~1");
            extensions.insert(format!("{prefix}/{key}"), value.clone());
        }
    }
}

#[derive(Debug, Error)]
pub enum StreamError {
    #[error(transparent)]
    Sse(#[from] SseDecodeError),
    #[error("Anthropic stream frame is not valid JSON: {0}")]
    Json(#[from] serde_json::Error),
    #[error("Anthropic stream event is missing type")]
    MissingEventType,
    #[error("SSE event name {event} does not match data type {data}")]
    EventTypeMismatch { event: String, data: String },
    #[error("Anthropic stream emitted data after message_stop")]
    DataAfterDone,
    #[error("Anthropic stream ended before message_stop")]
    UnexpectedEof,
    #[error("Anthropic stream contains duplicate message_start")]
    DuplicateMessageStart,
    #[error("unexpected Anthropic message type {0}")]
    UnexpectedMessageType(String),
    #[error("Anthropic message_start role is not assistant")]
    UnexpectedMessageRole,
    #[error("Anthropic message_start must have empty content and no stop reason")]
    InvalidMessageStartState,
    #[error("Anthropic stream event appeared before message_start")]
    EventBeforeMessageStart,
    #[error("Anthropic content block {0} started more than once")]
    DuplicateContentBlock(u32),
    #[error("Anthropic content block {0} is not open")]
    UnknownContentBlock(u32),
    #[error("Anthropic content delta is missing type")]
    MissingDeltaType,
    #[error("Anthropic content delta is missing {0}")]
    MissingDeltaField(&'static str),
    #[error("Anthropic {delta} delta does not match content block {index}")]
    DeltaBlockMismatch { index: u32, delta: String },
    #[error("Anthropic stream has too many tool calls")]
    TooManyToolCalls,
    #[error("Anthropic content event appeared after the message finish reason")]
    ContentAfterFinish,
    #[error("Anthropic stream emitted more than one finish reason")]
    DuplicateFinishReason,
    #[error("Anthropic message_stop arrived without a stop reason")]
    MessageStoppedWithoutFinishReason,
    #[error("Anthropic message_stop arrived with open blocks {0:?}")]
    MessageStoppedWithOpenBlocks(BTreeSet<u32>),
}
