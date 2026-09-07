use crate::protocols::openai::client::*;

#[test]
fn responses_stream_encoder_rejects_oversized_event_history() {
    let mut encoder = Encoder::new("route", "response", 0);
    let event = Event::new(
        0,
        Kind::TextDelta {
            output_index: 0,
            text: "x".repeat(16 * 1024 * 1024 + 1),
        },
    );

    assert!(matches!(
        encoder.push(event),
        Err(OpenAiClientEncodeError::EventHistoryTooLarge)
    ));
}

#[test]
fn responses_stream_encoder_charges_empty_deltas_against_event_history() {
    let mut encoder = Encoder::new("route", "response", 0);
    encoder.retained_bytes = MAX_RETAINED_BYTES;
    let event = Event::new(
        0,
        Kind::TextDelta {
            output_index: 0,
            text: String::new(),
        },
    );

    assert!(matches!(
        encoder.push(event),
        Err(OpenAiClientEncodeError::EventHistoryTooLarge)
    ));
}

#[test]
fn responses_stream_encoder_charges_unknown_finish_reasons_against_event_history() {
    let mut encoder = Encoder::new("route", "response", 0);
    encoder.retained_bytes = MAX_RETAINED_BYTES - std::mem::size_of::<Kind>();
    let event = Event::new(
        0,
        Kind::Finish {
            output_index: 0,
            reason: FinishReason::Other("x".to_owned()),
        },
    );

    assert!(matches!(
        encoder.push(event),
        Err(OpenAiClientEncodeError::EventHistoryTooLarge)
    ));
}
