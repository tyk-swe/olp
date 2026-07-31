use olp_conformance::{read_fixture, read_json};
use olp_domain::{CanonicalEvent, CanonicalEventKind, validate_event_sequence};
use olp_protocols::{
    anthropic::AnthropicMessagesStreamDecoder,
    gemini::GeminiGenerateContentStreamDecoder,
    openai::OpenAiChatStreamDecoder,
    sse::{SseDecoder, SseFrame},
};
use serde::Deserialize;

#[derive(Debug, Deserialize)]
struct GenericExpected {
    fragment_bytes: usize,
    frames: Vec<SseFrameFixture>,
}

#[derive(Debug, Deserialize)]
struct SseFrameFixture {
    event: Option<String>,
    data: String,
    id: Option<String>,
    retry_ms: Option<u64>,
}

#[derive(Debug, Deserialize)]
struct ProtocolExpected {
    fragment_bytes: usize,
    text: String,
    total_tokens: u64,
}

fn chunks(input: &[u8], size: usize) -> impl Iterator<Item = &[u8]> {
    assert!(size > 0, "fixture chunk size must be non-zero");
    input.chunks(size)
}

fn assert_protocol_events(events: &[CanonicalEvent], expected: &ProtocolExpected) {
    validate_event_sequence(events).expect("fixture must produce a valid canonical event sequence");
    let text = events
        .iter()
        .filter_map(|event| match &event.kind {
            CanonicalEventKind::TextDelta { text, .. } => Some(text.as_str()),
            _ => None,
        })
        .collect::<String>();
    assert_eq!(text, expected.text);
    assert_eq!(
        events
            .iter()
            .find_map(|event| match event.kind {
                CanonicalEventKind::Usage { usage } => Some(usage.total_tokens),
                _ => None,
            })
            .unwrap_or_default(),
        expected.total_tokens
    );
}

#[test]
fn generic_sse_survives_multiline_and_utf8_fragmentation() {
    let wire = read_fixture("streams/generic-fragmented.sse");
    let expected: GenericExpected = read_json("streams/generic-fragmented.expected.json");
    let mut decoder = SseDecoder::default();
    let mut actual = Vec::new();
    for chunk in chunks(&wire, expected.fragment_bytes) {
        actual.extend(decoder.push(chunk).expect("SSE chunk must decode"));
    }
    actual.extend(decoder.finish().expect("SSE EOF must decode"));

    let expected = expected
        .frames
        .into_iter()
        .map(|frame| SseFrame {
            event: frame.event,
            data: frame.data,
            id: frame.id,
            retry_ms: frame.retry_ms,
        })
        .collect::<Vec<_>>();
    assert_eq!(actual, expected);
}

#[test]
fn vendor_streams_survive_one_byte_fragmentation() {
    for stem in [
        "openai-chat",
        "anthropic-messages",
        "gemini-generate-content",
    ] {
        let wire = read_fixture(format!("streams/{stem}.sse"));
        let expected: ProtocolExpected = read_json(format!("streams/{stem}.expected.json"));

        let mut events = Vec::new();
        match stem {
            "openai-chat" => {
                let mut decoder = OpenAiChatStreamDecoder::new();
                for chunk in chunks(&wire, expected.fragment_bytes) {
                    events.extend(decoder.push(chunk).expect("OpenAI fixture must decode"));
                }
                events.extend(decoder.finish().expect("OpenAI fixture must finish"));
            }
            "anthropic-messages" => {
                let mut decoder = AnthropicMessagesStreamDecoder::new();
                for chunk in chunks(&wire, expected.fragment_bytes) {
                    events.extend(decoder.push(chunk).expect("Anthropic fixture must decode"));
                }
                events.extend(decoder.finish().expect("Anthropic fixture must finish"));
            }
            "gemini-generate-content" => {
                let mut decoder = GeminiGenerateContentStreamDecoder::new();
                for chunk in chunks(&wire, expected.fragment_bytes) {
                    events.extend(decoder.push(chunk).expect("Gemini fixture must decode"));
                }
                events.extend(decoder.finish().expect("Gemini fixture must finish"));
            }
            _ => unreachable!("the fixture list is exhaustive"),
        }
        assert_protocol_events(&events, &expected);
    }
}
