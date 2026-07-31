use std::{fmt, str};

use std::collections::BTreeMap;

use bytes::BytesMut;
use olp_domain::{CanonicalEvent, CanonicalEventKind, SourceExtensions, Surface};
use serde_json::{Value, json};
use thiserror::Error;

pub const DEFAULT_MAX_EVENT_BYTES: usize = 1024 * 1024;

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct SseFrame {
    pub event: Option<String>,
    pub data: String,
    pub id: Option<String>,
    pub retry_ms: Option<u64>,
}

pub(crate) const RAW_SSE_FRAME_EXTENSION: &str = "/__olp/raw_sse_frame";

pub(crate) fn raw_sse_frame_event(
    sequence: u64,
    surface: Surface,
    frame: &SseFrame,
    semantic_events: usize,
) -> CanonicalEvent {
    CanonicalEvent::new(
        sequence,
        CanonicalEventKind::SourceExtension {
            extensions: SourceExtensions::new(
                surface,
                BTreeMap::from([(
                    RAW_SSE_FRAME_EXTENSION.to_owned(),
                    json!({
                        "event": frame.event,
                        "data": frame.data,
                        "id": frame.id,
                        "retry_ms": frame.retry_ms,
                        "semantic_events": semantic_events,
                    }),
                )]),
            ),
        },
    )
}

pub(crate) fn decode_raw_sse_frame(value: &Value) -> Option<(SseFrame, usize)> {
    let object = value.as_object()?;
    let data = object.get("data")?.as_str()?.to_owned();
    let event = optional_string(object.get("event"))?;
    let id = optional_string(object.get("id"))?;
    let retry_ms = optional_u64(object.get("retry_ms"))?;
    let semantic_events = object.get("semantic_events")?.as_u64()?.try_into().ok()?;
    Some((
        SseFrame {
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

pub struct SseDecoder {
    buffer: BytesMut,
    event: Option<String>,
    data_lines: Vec<String>,
    has_data: bool,
    last_event_id: Option<String>,
    retry_ms: Option<u64>,
    pending_bytes: usize,
    max_event_bytes: usize,
    first_line: bool,
    skip_lf: bool,
}

impl fmt::Debug for SseDecoder {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SseDecoder")
            .field("buffered_bytes", &self.buffer.len())
            .field("data_line_count", &self.data_lines.len())
            .field("has_data", &self.has_data)
            .field("has_last_event_id", &self.last_event_id.is_some())
            .field("pending_bytes", &self.pending_bytes)
            .field("max_event_bytes", &self.max_event_bytes)
            .finish_non_exhaustive()
    }
}

impl Default for SseDecoder {
    fn default() -> Self {
        Self::new(DEFAULT_MAX_EVENT_BYTES)
    }
}

impl SseDecoder {
    #[must_use]
    pub fn new(max_event_bytes: usize) -> Self {
        Self {
            buffer: BytesMut::new(),
            event: None,
            data_lines: Vec::new(),
            has_data: false,
            last_event_id: None,
            retry_ms: None,
            pending_bytes: 0,
            max_event_bytes: max_event_bytes.max(1),
            first_line: true,
            skip_lf: false,
        }
    }

    pub fn push(&mut self, chunk: &[u8]) -> Result<Vec<SseFrame>, SseDecodeError> {
        let mut frames = Vec::new();
        let mut remaining = chunk;
        // Copy whole runs between terminators. Every streamed provider token
        // flows through here, so a per-byte append is the wrong constant
        // factor; CR / LF / CRLF handling is preserved by `skip_lf`.
        while !remaining.is_empty() {
            if self.skip_lf {
                self.skip_lf = false;
                if remaining[0] == b'\n' {
                    remaining = &remaining[1..];
                    continue;
                }
            }
            let Some(index) = remaining
                .iter()
                .position(|byte| matches!(byte, b'\r' | b'\n'))
            else {
                self.check_additional(self.buffer.len().saturating_add(remaining.len()))?;
                self.buffer.extend_from_slice(remaining);
                break;
            };
            self.check_additional(self.buffer.len().saturating_add(index).saturating_add(1))?;
            self.buffer.extend_from_slice(&remaining[..index]);
            let line_size = self.buffer.len();
            self.pending_bytes = self.pending_bytes.saturating_add(line_size + 1);
            let line = self.buffer.split();
            self.process_line(&line, &mut frames)?;
            self.skip_lf = remaining[index] == b'\r';
            remaining = &remaining[index + 1..];
        }
        Ok(frames)
    }

    pub fn finish(&mut self) -> Result<Vec<SseFrame>, SseDecodeError> {
        self.buffer.clear();
        self.event = None;
        self.data_lines.clear();
        self.has_data = false;
        self.retry_ms = None;
        self.pending_bytes = 0;
        self.skip_lf = false;
        Ok(Vec::new())
    }

    fn process_line(
        &mut self,
        line: &[u8],
        frames: &mut Vec<SseFrame>,
    ) -> Result<(), SseDecodeError> {
        let line = if self.first_line {
            self.first_line = false;
            line.strip_prefix(&[0xef, 0xbb, 0xbf]).unwrap_or(line)
        } else {
            line
        };
        if line.is_empty() {
            if let Some(frame) = self.dispatch() {
                frames.push(frame);
            }
            return Ok(());
        }
        if line[0] == b':' {
            return Ok(());
        }

        let line = str::from_utf8(line).map_err(SseDecodeError::InvalidUtf8)?;
        let (field, mut value) = line.split_once(':').unwrap_or((line, ""));
        if let Some(without_space) = value.strip_prefix(' ') {
            value = without_space;
        }

        match field {
            "event" => self.event = Some(value.to_owned()),
            "data" => {
                self.has_data = true;
                self.data_lines.push(value.to_owned());
            }
            "id" if !value.contains('\0') => self.last_event_id = Some(value.to_owned()),
            "retry" if !value.is_empty() && value.bytes().all(|byte| byte.is_ascii_digit()) => {
                self.retry_ms = value.parse().ok();
            }
            _ => {}
        }
        Ok(())
    }

    fn dispatch(&mut self) -> Option<SseFrame> {
        self.pending_bytes = 0;
        let event = self.event.take();
        let retry_ms = self.retry_ms.take();
        if !self.has_data {
            self.data_lines.clear();
            return None;
        }

        self.has_data = false;
        Some(SseFrame {
            event,
            data: self.data_lines.drain(..).collect::<Vec<_>>().join("\n"),
            id: self.last_event_id.clone(),
            retry_ms,
        })
    }

    fn check_additional(&self, additional: usize) -> Result<(), SseDecodeError> {
        let actual = self.pending_bytes.saturating_add(additional);
        if actual > self.max_event_bytes {
            return Err(SseDecodeError::EventTooLarge {
                maximum: self.max_event_bytes,
                actual,
            });
        }
        Ok(())
    }
}

pub fn encode_frame(frame: &SseFrame) -> Result<Vec<u8>, SseEncodeError> {
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
            return Err(SseEncodeError::NullId);
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
    let normalized_data = frame.data.replace("\r\n", "\n").replace('\r', "\n");
    for line in normalized_data.split('\n') {
        encoded.extend_from_slice(b"data: ");
        encoded.extend_from_slice(line.as_bytes());
        encoded.push(b'\n');
    }
    encoded.push(b'\n');
    Ok(encoded)
}

fn validate_single_line(field: &'static str, value: &str) -> Result<(), SseEncodeError> {
    if value.contains(['\r', '\n']) {
        return Err(SseEncodeError::MultilineField { field });
    }
    Ok(())
}

#[derive(Debug, Error)]
pub enum SseDecodeError {
    #[error("SSE event exceeds {maximum} byte limit ({actual} bytes buffered)")]
    EventTooLarge { maximum: usize, actual: usize },
    #[error("SSE line is not valid UTF-8")]
    InvalidUtf8(#[source] str::Utf8Error),
}

#[derive(Clone, Copy, Debug, Error, Eq, PartialEq)]
pub enum SseEncodeError {
    #[error("SSE {field} field must fit on one line")]
    MultilineField { field: &'static str },
    #[error("SSE event ID cannot contain a null character")]
    NullId,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn decoder_accepts_bom_and_all_event_stream_line_endings() {
        let mut decoder = SseDecoder::default();
        assert!(decoder.push(&[0xef]).unwrap().is_empty());
        let frames = decoder.push(b"\xbb\xbfdata: one\rdata: two\r\r").unwrap();
        assert_eq!(frames.len(), 1);
        assert_eq!(frames[0].data, "one\ntwo");

        let frames = decoder.push(b"data: three\r\ndata: four\n\n").unwrap();
        assert_eq!(frames.len(), 1);
        assert_eq!(frames[0].data, "three\nfour");

        let mut decoder = SseDecoder::default();
        assert!(
            decoder
                .push(b"\n\xef\xbb\xbfdata: not-a-bom\n\n")
                .unwrap()
                .is_empty()
        );
    }

    #[test]
    fn decoder_discards_an_incomplete_event_at_eof() {
        let mut decoder = SseDecoder::default();
        assert!(decoder.push(b"data: incomplete\n").unwrap().is_empty());
        assert!(decoder.finish().unwrap().is_empty());
    }
}
