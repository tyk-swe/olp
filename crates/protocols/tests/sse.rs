use olp_protocols::sse::{SseDecodeError, SseDecoder, SseFrame, encode_frame};
use proptest::prelude::*;

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
fn finish_does_not_count_an_unterminated_line_twice() {
    let mut decoder = SseDecoder::new(7);
    assert!(decoder.push(b"data: x").unwrap().is_empty());
    assert_eq!(
        decoder.finish().unwrap(),
        vec![SseFrame {
            data: "x".into(),
            ..SseFrame::default()
        }]
    );
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
        let mut actual = Vec::new();
        let mut decoder = SseDecoder::default();
        let mut offset = 0;
        let mut widths = widths.into_iter().cycle();
        while offset < wire.len() {
            let width = widths.next().unwrap();
            let end = (offset + width).min(wire.len());
            actual.extend(decoder.push(&wire[offset..end]).unwrap());
            offset = end;
        }
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
