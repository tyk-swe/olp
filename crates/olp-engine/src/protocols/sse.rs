use std::{borrow::Cow, fmt, str};

use std::collections::BTreeMap;

use crate::domain::canonical::{
    events::{Event, Kind},
    identity::Surface,
    requests::SourceExtensions,
};
use bytes::BytesMut;
use serde_json::Value;
use thiserror::Error;

pub const DEFAULT_MAX_EVENT_BYTES: usize = 1024 * 1024;

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Frame {
    pub event: Option<String>,
    pub data: String,
    pub id: Option<String>,
    pub retry_ms: Option<u64>,
}

pub(in crate::protocols) const RAW_SSE_FRAME_EXTENSION: &str = "/__olp/raw_sse_frame";

#[derive(Clone, Copy)]
enum TrailingCr {
    None,
    Processed { provisional_lf_byte: bool },
    DeferredLine,
}

/// Wraps a raw upstream frame for passthrough. The frame is moved, not
/// cloned: this runs once per streamed token on the passthrough path.
pub(in crate::protocols) fn raw_sse_frame_event(
    sequence: u64,
    surface: Surface,
    frame: Frame,
    semantic_events: usize,
) -> Event {
    let mut raw = serde_json::Map::with_capacity(5);
    raw.insert(
        "event".into(),
        frame.event.map_or(Value::Null, Value::String),
    );
    raw.insert("data".into(), Value::String(frame.data));
    raw.insert("id".into(), frame.id.map_or(Value::Null, Value::String));
    raw.insert(
        "retry_ms".into(),
        frame.retry_ms.map_or(Value::Null, Value::from),
    );
    raw.insert("semantic_events".into(), Value::from(semantic_events));
    Event::new(
        sequence,
        Kind::SourceExtension {
            extensions: SourceExtensions::new(
                surface,
                BTreeMap::from([(RAW_SSE_FRAME_EXTENSION.to_owned(), Value::Object(raw))]),
            ),
        },
    )
}

/// Places a non-error raw frame ahead of the semantic events it produced and
/// shifts their sequence numbers by one. Error frames stay canonical so
/// routing can observe failures before committing a response.
pub(in crate::protocols) fn insert_raw_frame(
    events: &mut Vec<Event>,
    event_start: usize,
    sequence_start: u64,
    surface: Surface,
    frame: Frame,
    next_sequence: &mut u64,
) {
    if events[event_start..]
        .iter()
        .any(|event| matches!(&event.kind, Kind::Error { .. }))
    {
        return;
    }
    let semantic_events = events.len().saturating_sub(event_start);
    for event in &mut events[event_start..] {
        event.sequence = event.sequence.saturating_add(1);
    }
    events.insert(
        event_start,
        raw_sse_frame_event(sequence_start, surface, frame, semantic_events),
    );
    *next_sequence = next_sequence.saturating_add(1);
}

/// Inverse of [`raw_sse_frame_event`]; consumes the extension value so the
/// frame data is moved back out rather than copied.
pub(in crate::protocols) fn decode_raw_sse_frame(value: Value) -> Option<(Frame, usize)> {
    let Value::Object(mut object) = value else {
        return None;
    };
    let Value::String(data) = object.remove("data")? else {
        return None;
    };
    let event = optional_string(object.get("event"))?;
    let id = optional_string(object.get("id"))?;
    let retry_ms = optional_u64(object.get("retry_ms"))?;
    let semantic_events = object.get("semantic_events")?.as_u64()?.try_into().ok()?;
    Some((
        Frame {
            event,
            data,
            id,
            retry_ms,
        },
        semantic_events,
    ))
}

fn optional_string(value: Option<&Value>) -> Option<Option<String>> {
    match value {
        None | Some(Value::Null) => Some(None),
        Some(Value::String(value)) => Some(Some(value.clone())),
        Some(_) => None,
    }
}

fn optional_u64(value: Option<&Value>) -> Option<Option<u64>> {
    match value {
        None | Some(Value::Null) => Some(None),
        Some(Value::Number(value)) => value.as_u64().map(Some),
        Some(_) => None,
    }
}

pub struct Decoder {
    // WHATWG permits one leading UTF-8 BOM, including across chunks.
    bom_checked: bool,
    buffer: BytesMut,
    trailing_cr: TrailingCr,
    event: Option<String>,
    // Joined with '\n' as data lines arrive; `has_data` distinguishes an
    // empty payload (one bare `data:` line) from no data lines at all.
    data: String,
    has_data: bool,
    last_event_id: Option<String>,
    retry_ms: Option<u64>,
    pending_bytes: usize,
    max_event_bytes: usize,
}

impl fmt::Debug for Decoder {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("Decoder")
            .field("buffered_bytes", &self.buffer.len())
            .field("data_bytes", &self.data.len())
            .field("has_data", &self.has_data)
            .field("has_last_event_id", &self.last_event_id.is_some())
            .field("pending_bytes", &self.pending_bytes)
            .field("max_event_bytes", &self.max_event_bytes)
            .finish_non_exhaustive()
    }
}

impl Default for Decoder {
    fn default() -> Self {
        Self::new(DEFAULT_MAX_EVENT_BYTES)
    }
}

impl Decoder {
    #[must_use]
    pub fn new(max_event_bytes: usize) -> Self {
        Self {
            buffer: BytesMut::new(),
            trailing_cr: TrailingCr::None,
            event: None,
            data: String::new(),
            has_data: false,
            last_event_id: None,
            retry_ms: None,
            pending_bytes: 0,
            max_event_bytes: max_event_bytes.max(1),
            bom_checked: false,
        }
    }

    pub fn push(&mut self, chunk: &[u8]) -> Result<Vec<Frame>, DecodeError> {
        let mut frames = Vec::new();
        let mut remaining = self.resolve_trailing_cr(chunk, &mut frames)?;

        // WHATWG leading BOM handling must wait until three bytes are
        // available when the BOM is split across transport chunks.
        if !self.bom_checked {
            const UTF8_BOM: [u8; 3] = [0xef, 0xbb, 0xbf];
            if self.buffer.is_empty() && remaining.is_empty() {
                return Ok(frames);
            }
            while self.buffer.len() < UTF8_BOM.len()
                && !remaining.is_empty()
                && self.buffer[..] == UTF8_BOM[..self.buffer.len()]
                && remaining[0] == UTF8_BOM[self.buffer.len()]
            {
                self.buffer.extend_from_slice(&remaining[..1]);
                remaining = &remaining[1..];
            }
            let prefix_len = self.buffer.len().min(UTF8_BOM.len());

            if prefix_len > 0 && self.buffer[..prefix_len] == UTF8_BOM[..prefix_len] {
                if self.buffer.len() < UTF8_BOM.len() && remaining.is_empty() {
                    return Ok(Vec::new());
                }
                if self.buffer.len() == UTF8_BOM.len() {
                    let _ = self.buffer.split_to(UTF8_BOM.len());
                }
            }

            self.bom_checked = true;
        }

        while let Some(line_end) = remaining
            .iter()
            .position(|byte| matches!(*byte, b'\r' | b'\n'))
        {
            let terminator = remaining[line_end];
            let line_size = self.buffer.len().saturating_add(line_end);
            let next_is_lf =
                terminator == b'\r' && remaining.get(line_end.saturating_add(1)) == Some(&b'\n');
            let trailing_cr = terminator == b'\r' && line_end.saturating_add(1) == remaining.len();

            if trailing_cr {
                let cr_size = line_size.saturating_add(1);
                self.check_additional(cr_size)?;
                let crlf_size = line_size.saturating_add(2);
                if self.check_additional(crlf_size).is_err() {
                    // The CR fits but a CRLF would not. Delay only this
                    // boundary case until the next byte (or EOF) resolves it.
                    self.buffer.extend_from_slice(&remaining[..line_end]);
                    self.trailing_cr = TrailingCr::DeferredLine;
                    remaining = &[];
                    break;
                }

                self.buffer.extend_from_slice(&remaining[..line_end]);
                let provisional_lf_byte = line_size != 0;
                self.process_buffered_line(2, &mut frames)?;
                // Processing an empty line resets the event accounting, so
                // only a non-empty line can still carry the provisional byte.
                self.trailing_cr = TrailingCr::Processed {
                    provisional_lf_byte,
                };
                remaining = &[];
                break;
            }

            let terminator_size = if next_is_lf { 2 } else { 1 };
            self.check_additional(line_size.saturating_add(terminator_size))?;
            self.buffer.extend_from_slice(&remaining[..line_end]);
            self.process_buffered_line(terminator_size, &mut frames)?;
            remaining = &remaining[line_end + terminator_size..];
        }

        self.check_additional(self.buffer.len().saturating_add(remaining.len()))?;
        self.buffer.extend_from_slice(remaining);
        Ok(frames)
    }

    pub fn finish(&mut self) -> Result<Vec<Frame>, DecodeError> {
        // EOF is not an SSE event delimiter. Discard an
        // unterminated line and any event awaiting a blank line.
        let mut frames = Vec::new();
        match std::mem::replace(&mut self.trailing_cr, TrailingCr::None) {
            TrailingCr::Processed {
                provisional_lf_byte: true,
            } => self.pending_bytes = self.pending_bytes.saturating_sub(1),
            TrailingCr::DeferredLine => self.process_buffered_line(1, &mut frames)?,
            TrailingCr::None
            | TrailingCr::Processed {
                provisional_lf_byte: false,
            } => {}
        }
        self.buffer.clear();
        let _ = self.dispatch();
        Ok(frames)
    }

    fn resolve_trailing_cr<'a>(
        &mut self,
        chunk: &'a [u8],
        frames: &mut Vec<Frame>,
    ) -> Result<&'a [u8], DecodeError> {
        if chunk.is_empty() {
            return Ok(chunk);
        }

        let next_is_lf = chunk[0] == b'\n';
        match std::mem::replace(&mut self.trailing_cr, TrailingCr::None) {
            TrailingCr::None => {}
            TrailingCr::Processed {
                provisional_lf_byte,
            } => {
                if next_is_lf {
                    return Ok(&chunk[1..]);
                }
                if provisional_lf_byte {
                    self.pending_bytes = self.pending_bytes.saturating_sub(1);
                }
            }
            TrailingCr::DeferredLine => {
                self.process_buffered_line(if next_is_lf { 2 } else { 1 }, frames)?;
                if next_is_lf {
                    return Ok(&chunk[1..]);
                }
            }
        }
        Ok(chunk)
    }

    fn process_buffered_line(
        &mut self,
        terminator_size: usize,
        frames: &mut Vec<Frame>,
    ) -> Result<(), DecodeError> {
        let accounted_size = self.buffer.len().saturating_add(terminator_size);
        self.check_additional(accounted_size)?;
        self.pending_bytes = self.pending_bytes.saturating_add(accounted_size);
        let line = self.buffer.split();
        self.process_line(&line, frames)
    }

    fn process_line(&mut self, line: &[u8], frames: &mut Vec<Frame>) -> Result<(), DecodeError> {
        if line.is_empty() {
            if let Some(frame) = self.dispatch() {
                frames.push(frame);
            }
            return Ok(());
        }
        if line[0] == b':' {
            return Ok(());
        }

        let line = str::from_utf8(line).map_err(DecodeError::InvalidUtf8)?;
        let (field, mut value) = line.split_once(':').unwrap_or((line, ""));
        if let Some(without_space) = value.strip_prefix(' ') {
            value = without_space;
        }

        match field {
            "event" => self.event = Some(value.to_owned()),
            "data" => {
                if self.has_data {
                    self.data.push('\n');
                }
                self.has_data = true;
                self.data.push_str(value);
            }
            "id" if !value.contains('\0') => self.last_event_id = Some(value.to_owned()),
            "retry" if !value.is_empty() && value.bytes().all(|byte| byte.is_ascii_digit()) => {
                self.retry_ms = value.parse().ok();
            }
            _ => {}
        }
        Ok(())
    }

    fn dispatch(&mut self) -> Option<Frame> {
        self.pending_bytes = 0;
        let event = self.event.take();
        let retry_ms = self.retry_ms.take();
        if !self.has_data {
            self.data.clear();
            return None;
        }

        self.has_data = false;
        Some(Frame {
            event,
            data: std::mem::take(&mut self.data),
            id: self.last_event_id.clone(),
            retry_ms,
        })
    }

    fn check_additional(&self, additional: usize) -> Result<(), DecodeError> {
        let actual = self.pending_bytes.saturating_add(additional);
        if actual > self.max_event_bytes {
            return Err(DecodeError::EventTooLarge {
                maximum: self.max_event_bytes,
                actual,
            });
        }
        Ok(())
    }
}

pub fn encode_frame(frame: &Frame) -> Result<Vec<u8>, EncodeError> {
    let mut encoded = Vec::new();
    if let Some(event) = &frame.event {
        validate_single_line("event", event)?;
        encoded.extend_from_slice(b"event: ");
        encoded.extend_from_slice(event.as_bytes());
        encoded.push(b'\n');
    }
    if let Some(id) = &frame.id {
        validate_single_line("id", id)?;
        if id.contains('\0') {
            return Err(EncodeError::NullId);
        }
        encoded.extend_from_slice(b"id: ");
        encoded.extend_from_slice(id.as_bytes());
        encoded.push(b'\n');
    }
    if let Some(retry_ms) = frame.retry_ms {
        encoded.extend_from_slice(format!("retry: {retry_ms}\n").as_bytes());
    }
    // Event streams normalize CR, LF, and CRLF line endings. Emitting a raw
    // carriage return inside a data field would let a conforming client parse
    // the remainder as a new SSE field instead of payload data.
    let normalized_data: Cow<'_, str> = if frame.data.contains('\r') {
        Cow::Owned(frame.data.replace("\r\n", "\n").replace('\r', "\n"))
    } else {
        Cow::Borrowed(&frame.data)
    };
    for line in normalized_data.split('\n') {
        encoded.extend_from_slice(b"data: ");
        encoded.extend_from_slice(line.as_bytes());
        encoded.push(b'\n');
    }
    encoded.push(b'\n');
    Ok(encoded)
}

fn validate_single_line(field: &'static str, value: &str) -> Result<(), EncodeError> {
    if value.contains(['\r', '\n']) {
        return Err(EncodeError::MultilineField { field });
    }
    Ok(())
}

#[derive(Debug, Error)]
pub enum DecodeError {
    #[error("SSE event exceeds {maximum} byte limit ({actual} bytes buffered)")]
    EventTooLarge { maximum: usize, actual: usize },
    #[error("SSE line is not valid UTF-8")]
    InvalidUtf8(#[source] str::Utf8Error),
}

#[derive(Clone, Copy, Debug, Error, Eq, PartialEq)]
pub enum EncodeError {
    #[error("SSE {field} field must fit on one line")]
    MultilineField { field: &'static str },
    #[error("SSE event ID cannot contain a null character")]
    NullId,
}
