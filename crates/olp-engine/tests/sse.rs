use olp_engine::protocols::sse::{SseDecodeError, SseDecoder, SseFrame, encode_frame};
use proptest::prelude::*;

fn decode_fragmented(
    wire: &[u8],
    widths: &[usize],
    finish: bool,
) -> Result<Vec<SseFrame>, SseDecodeError> {
    let mut frames = Vec::new();
    let mut decoder = SseDecoder::default();
    let mut offset = 0;
    let mut widths = widths.iter().copied().cycle();
    while offset < wire.len() {
        let width = widths.next().expect("property always supplies a width");
        let end = (offset + width).min(wire.len());
        frames.extend(decoder.push(&wire[offset..end])?);
        offset = end;
    }
    if finish {
        frames.extend(decoder.finish()?);
    }
    Ok(frames)
}

#[test]
fn multiline_crlf_comments_and_persistent_ids_follow_sse_rules() {
    let wire = b": keepalive\r\nid: event-7\r\nevent: message\r\ndata: first\r\ndata: second\r\nretry: 250\r\n\r\ndata: next\r\n\r\n";
    let frames = SseDecoder::default().push(wire).unwrap();
    assert_eq!(
        frames,
        vec![
            SseFrame {
                event: Some("message".into()),
                data: "first\nsecond".into(),
                id: Some("event-7".into()),
                retry_ms: Some(250),
            },
            SseFrame {
                event: None,
                data: "next".into(),
                id: Some("event-7".into()),
                retry_ms: None,
            },
        ]
    );
}

#[test]
fn cr_only_line_endings_dispatch_independent_events() {
    let wire = b"id: event-7\rdata: first\rdata: second\r\rdata: next\r\r";
    let frames = SseDecoder::default().push(wire).unwrap();
    assert_eq!(
        frames,
        vec![
            SseFrame {
                data: "first\nsecond".into(),
                id: Some("event-7".into()),
                ..SseFrame::default()
            },
            SseFrame {
                data: "next".into(),
                id: Some("event-7".into()),
                ..SseFrame::default()
            },
        ]
    );
}

#[test]
fn cr_only_event_limit_counts_exact_wire_bytes() {
    const WIRE: &[u8] = b"data: x\r\r";
    let expected = SseFrame {
        data: "x".into(),
        ..SseFrame::default()
    };

    let mut below_limit = SseDecoder::new(WIRE.len() - 1);
    assert!(matches!(
        below_limit.push(WIRE),
        Err(SseDecodeError::EventTooLarge {
            maximum: 8,
            actual: 9,
        })
    ));

    let mut at_limit = SseDecoder::new(WIRE.len());
    assert!(at_limit.push(WIRE).unwrap().is_empty());
    assert_eq!(at_limit.finish().unwrap(), vec![expected.clone()]);

    let mut fragmented = SseDecoder::new(WIRE.len());
    assert!(fragmented.push(b"data: x\r").unwrap().is_empty());
    assert!(fragmented.push(b"\r").unwrap().is_empty());
    assert_eq!(fragmented.finish().unwrap(), vec![expected]);
}

#[test]
fn crlf_split_across_chunks_is_one_line_ending() {
    let mut decoder = SseDecoder::default();
    assert!(decoder.push(b"data: first\r").unwrap().is_empty());
    assert_eq!(
        decoder.push(b"\n\r").unwrap(),
        vec![SseFrame {
            data: "first".into(),
            ..SseFrame::default()
        }]
    );
    assert!(decoder.push(b"\ndata: second\r\n").unwrap().is_empty());
    assert_eq!(
        decoder.push(b"\r").unwrap(),
        vec![SseFrame {
            data: "second".into(),
            ..SseFrame::default()
        }]
    );
    assert!(decoder.push(b"\n").unwrap().is_empty());
}

#[test]
fn encoder_round_trips_multiline_unicode_data() {
    let frame = SseFrame {
        event: Some("delta".into()),
        data: "héllo\n世界".into(),
        id: Some("42".into()),
        retry_ms: Some(500),
    };
    let encoded = encode_frame(&frame).unwrap();
    let decoded = SseDecoder::default().push(&encoded).unwrap();
    assert_eq!(decoded, vec![frame]);
}

#[test]
fn encoder_normalizes_carriage_returns_in_data_without_field_injection() {
    let frame = SseFrame {
        data: "first\rsecond\r\nthird".into(),
        ..SseFrame::default()
    };
    let encoded = encode_frame(&frame).unwrap();
    assert!(!encoded.contains(&b'\r'));
    assert_eq!(
        SseDecoder::default().push(&encoded).unwrap(),
        vec![SseFrame {
            data: "first\nsecond\nthird".into(),
            ..SseFrame::default()
        }]
    );
}

#[test]
fn configured_event_limit_bounds_unterminated_input() {
    let mut decoder = SseDecoder::new(8);
    assert!(matches!(
        decoder.push(b"data: this input never terminates"),
        Err(SseDecodeError::EventTooLarge { maximum: 8, .. })
    ));
}

#[test]
fn event_limit_applies_per_event_not_per_transport_chunk() {
    let mut decoder = SseDecoder::new(16);
    let frames = decoder.push(b"data: a\n\ndata: b\n\ndata: c\n\n").unwrap();
    assert_eq!(frames.len(), 3);
}

#[test]
fn contiguous_crlf_counts_both_bytes_at_the_event_limit() {
    const WIRE: &[u8] = b"data: x\r\n\r\n";

    let mut below_limit = SseDecoder::new(WIRE.len() - 1);
    assert!(matches!(
        below_limit.push(WIRE),
        Err(SseDecodeError::EventTooLarge {
            maximum: 10,
            actual: 11,
        })
    ));

    let mut at_limit = SseDecoder::new(WIRE.len());
    assert_eq!(
        at_limit.push(WIRE).unwrap(),
        vec![SseFrame {
            data: "x".into(),
            ..SseFrame::default()
        }]
    );
}

#[test]
fn split_crlf_has_the_same_event_limit_accounting() {
    const WIRE_LEN: usize = b"data: x\r\n\r\n".len();

    let mut below_limit = SseDecoder::new(WIRE_LEN - 1);
    assert!(below_limit.push(b"data: x\r").unwrap().is_empty());
    assert!(below_limit.push(b"\n\r").unwrap().is_empty());
    assert!(matches!(
        below_limit.push(b"\n"),
        Err(SseDecodeError::EventTooLarge {
            maximum: 10,
            actual: 11,
        })
    ));

    let mut at_limit = SseDecoder::new(WIRE_LEN);
    assert!(at_limit.push(b"data: x\r").unwrap().is_empty());
    assert_eq!(
        at_limit.push(b"\n\r").unwrap(),
        vec![SseFrame {
            data: "x".into(),
            ..SseFrame::default()
        }]
    );
    assert!(at_limit.push(b"\n").unwrap().is_empty());
}

#[test]
fn finish_does_not_count_an_unterminated_line_twice() {
    let mut decoder = SseDecoder::new(7);
    assert!(decoder.push(b"data: x").unwrap().is_empty());
    assert!(decoder.finish().unwrap().is_empty());
}

#[test]
fn decoder_debug_output_does_not_expose_buffered_content() {
    let mut decoder = SseDecoder::default();
    decoder.push(b"data: private output marker").unwrap();
    let debug = format!("{decoder:?}");
    assert!(debug.contains("buffered_bytes"));
    assert!(!debug.contains("private output marker"));
}

proptest! {
    #![proptest_config(ProptestConfig::with_cases(64))]

    #[test]
    fn arbitrary_fragmentation_does_not_change_decoding(widths in prop::collection::vec(1_usize..16, 1..40)) {
        let wire = "event: token\ndata: héllø 🌍\n\ndata: second\n\n".as_bytes();
        let expected = SseDecoder::default().push(wire).unwrap();
        let actual = decode_fragmented(wire, &widths, false).unwrap();
        prop_assert_eq!(actual, expected);
    }

    #[test]
    fn arbitrary_crlf_fragmentation_does_not_change_decoding(widths in prop::collection::vec(1_usize..16, 1..40)) {
        let wire = "event: token\r\ndata: héllø 🌍\r\n\r\ndata: second\r\n\r\n".as_bytes();
        let expected = SseDecoder::default().push(wire).unwrap();
        let actual = decode_fragmented(wire, &widths, false).unwrap();
        prop_assert_eq!(actual, expected);
    }

    #[test]
    fn arbitrary_cr_fragmentation_does_not_change_decoding(widths in prop::collection::vec(1_usize..16, 1..40)) {
        let wire = "event: token\rdata: héllø 🌍\r\rdata: second\r\r".as_bytes();
        let mut contiguous = SseDecoder::default();
        let mut expected = contiguous.push(wire).unwrap();
        expected.extend(contiguous.finish().unwrap());
        let actual = decode_fragmented(wire, &widths, true).unwrap();
        prop_assert_eq!(actual, expected);
    }

    #[test]
    fn arbitrary_unicode_data_round_trips_through_encoder(data in "[^\\r]{0,512}") {
        let frame = SseFrame {
            event: Some("property".into()),
            data,
            id: Some("event-id".into()),
            retry_ms: Some(250),
        };
        let wire = encode_frame(&frame).unwrap();
        let decoded = SseDecoder::default().push(&wire).unwrap();
        prop_assert_eq!(decoded, vec![frame]);
    }
}
