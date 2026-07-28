//! Independent server-sent-events decoder written directly to the WHATWG
//! event-stream specification. The product has its own SSE encoder and
//! decoder; decoding gateway responses with an independent implementation
//! keeps a product decoder bug from masking a product encoder bug.

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct SseEvent {
    /// Event type buffer; empty means the default `message` type.
    pub event: String,
    /// Data lines joined with a single newline, per specification.
    pub data: String,
    pub id: Option<String>,
    pub retry: Option<String>,
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct SseStream {
    pub events: Vec<SseEvent>,
    /// Field lines buffered after the final dispatched event. A conforming
    /// producer ends every event with a blank line, so this must be empty.
    pub undispatched_tail: Vec<String>,
}

pub fn decode(bytes: &[u8]) -> Result<SseStream, String> {
    let text =
        std::str::from_utf8(bytes).map_err(|error| format!("stream is not UTF-8: {error}"))?;
    let text = text.strip_prefix('\u{feff}').unwrap_or(text);

    let mut stream = SseStream::default();
    let mut event_type = String::new();
    let mut data_lines: Vec<&str> = Vec::new();
    let mut id = None;
    let mut retry = None;
    let mut raw_since_dispatch: Vec<String> = Vec::new();

    for line in split_spec_lines(text) {
        if line.is_empty() {
            // Dispatch: with an empty data buffer the event is discarded and
            // both buffers reset, per specification.
            if data_lines.is_empty() {
                event_type.clear();
            } else {
                stream.events.push(SseEvent {
                    event: std::mem::take(&mut event_type),
                    data: data_lines.join("\n"),
                    id: id.clone(),
                    retry: retry.take(),
                });
                data_lines.clear();
            }
            raw_since_dispatch.clear();
            continue;
        }
        raw_since_dispatch.push(line.to_owned());
        if line.starts_with(':') {
            continue;
        }
        let (field, value) = match line.split_once(':') {
            Some((field, value)) => (field, value.strip_prefix(' ').unwrap_or(value)),
            None => (line, ""),
        };
        match field {
            "event" => event_type = value.to_owned(),
            "data" => data_lines.push(value),
            "id" if !value.contains('\0') => id = Some(value.to_owned()),
            "retry" => retry = Some(value.to_owned()),
            _ => {}
        }
    }
    stream.undispatched_tail = raw_since_dispatch;
    Ok(stream)
}

/// Splits on the three specification line terminators: CRLF, LF, and lone CR.
fn split_spec_lines(text: &str) -> Vec<&str> {
    let mut lines = Vec::new();
    let bytes = text.as_bytes();
    let mut start = 0;
    let mut index = 0;
    while index < bytes.len() {
        match bytes[index] {
            b'\n' => {
                lines.push(&text[start..index]);
                index += 1;
                start = index;
            }
            b'\r' => {
                lines.push(&text[start..index]);
                index += 1;
                if bytes.get(index) == Some(&b'\n') {
                    index += 1;
                }
                start = index;
            }
            _ => index += 1,
        }
    }
    if start < bytes.len() {
        lines.push(&text[start..]);
    }
    lines
}
