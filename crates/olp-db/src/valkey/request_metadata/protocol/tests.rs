use redis::Value;

use super::*;

fn bulk(value: &str) -> Value {
    Value::BulkString(value.as_bytes().to_vec())
}

fn entry(id: &str, fields: Vec<Value>) -> Value {
    Value::Array(vec![bulk(id), Value::Array(fields)])
}

#[test]
fn xread_parser_accepts_exact_resp2_and_resp3_shapes() {
    let resp2 = Value::Array(vec![Value::Array(vec![
        bulk("installation:request-metadata"),
        Value::Array(vec![entry(
            "1713465533411-0",
            vec![bulk("event"), bulk("{\"event_id\":\"opaque\"}")],
        )]),
    ])]);
    let entries = parse_xread_reply(resp2, "installation:request-metadata", 10).unwrap();
    assert_eq!(entries.len(), 1);
    assert_eq!(entries[0].id, "1713465533411-0");
    assert_eq!(
        entries[0].payload.as_deref(),
        Some(b"{\"event_id\":\"opaque\"}".as_slice())
    );
    assert!(entries[0].deleted_pending_id.is_none());

    let resp3 = Value::Map(vec![(
        bulk("installation:request-metadata"),
        Value::Array(vec![Value::Array(vec![
            bulk("1713465533412-0"),
            Value::Map(vec![(bulk("event"), bulk("{}"))]),
        ])]),
    )]);
    let entries = parse_xread_reply(resp3, "installation:request-metadata", 10).unwrap();
    assert_eq!(entries[0].payload.as_deref(), Some(b"{}".as_slice()));
}

#[test]
fn xread_parser_recovers_durable_deleted_pending_markers() {
    let reply = Value::Array(vec![Value::Array(vec![
        bulk("stream"),
        Value::Array(vec![entry(
            "1713465533411-0",
            vec![bulk("deleted_pending_id"), bulk("1713465532000-0")],
        )]),
    ])]);
    let entries = parse_xread_reply(reply, "stream", 1).unwrap();

    assert!(entries[0].payload.is_none());
    assert_eq!(
        entries[0].deleted_pending_id.as_deref(),
        Some("1713465532000-0")
    );
}

#[test]
fn xread_parser_preserves_semantically_malformed_entries_for_gap_handling() {
    for fields in [
        Value::Nil,
        Value::Array(vec![bulk("other"), bulk("{}")]),
        Value::Array(vec![
            bulk("event"),
            bulk("{}"),
            bulk("extra"),
            bulk("content-is-never-read"),
        ]),
        Value::Array(vec![bulk("event"), Value::Int(7)]),
    ] {
        let reply = Value::Array(vec![Value::Array(vec![
            bulk("stream"),
            Value::Array(vec![Value::Array(vec![bulk("1-0"), fields])]),
        ])]);
        let entries = parse_xread_reply(reply, "stream", 1).unwrap();
        assert!(entries[0].payload.is_none());
        assert!(entries[0].deleted_pending_id.is_none());
    }
}

#[test]
fn xread_parser_rejects_ambiguous_or_unbounded_protocol_replies() {
    let wrong_stream = Value::Array(vec![Value::Array(vec![
        bulk("other"),
        Value::Array(Vec::new()),
    ])]);
    assert!(parse_xread_reply(wrong_stream, "expected", 10).is_err());

    let duplicate_id = Value::Array(vec![Value::Array(vec![
        bulk("stream"),
        Value::Array(vec![
            entry("1-0", vec![bulk("event"), bulk("{}")]),
            entry("1-0", vec![bulk("event"), bulk("{}")]),
        ]),
    ])]);
    assert!(parse_xread_reply(duplicate_id, "stream", 10).is_err());

    let oversized = Value::Array(vec![Value::Array(vec![
        bulk("stream"),
        Value::Array(vec![
            entry("1-0", vec![bulk("event"), bulk("{}")]),
            entry("2-0", vec![bulk("event"), bulk("{}")]),
        ]),
    ])]);
    assert!(parse_xread_reply(oversized, "stream", 1).is_err());

    let odd_fields = Value::Array(vec![Value::Array(vec![
        bulk("stream"),
        Value::Array(vec![entry("1-0", vec![bulk("event")])]),
    ])]);
    assert!(parse_xread_reply(odd_fields, "stream", 10).is_err());

    let invalid_id = Value::Array(vec![Value::Array(vec![
        bulk("stream"),
        Value::Array(vec![entry("not-an-id", vec![bulk("event"), bulk("{}")])]),
    ])]);
    assert!(parse_xread_reply(invalid_id, "stream", 10).is_err());
}

#[test]
fn xautoclaim_parser_strictly_validates_claimed_and_deleted_entries() {
    let reply = Value::Array(vec![
        bulk("1713465536578-0"),
        Value::Array(vec![entry(
            "1713465533411-0",
            vec![bulk("event"), bulk("{}")],
        )]),
        Value::Array(vec![bulk("1713465532000-0")]),
    ]);
    let page = parse_auto_claim_reply(reply, 10).unwrap();
    assert_eq!(page.next_start, "1713465536578-0");
    assert_eq!(page.entries.len(), 1);
    assert_eq!(page.deleted_ids, ["1713465532000-0"]);

    let legacy_shape = Value::Array(vec![bulk("0-0"), Value::Array(Vec::new())]);
    let page = parse_auto_claim_reply(legacy_shape, 10).unwrap();
    assert_eq!(page.next_start, "0-0");
    assert!(page.entries.is_empty());
    assert!(page.deleted_ids.is_empty());
}

#[test]
fn xautoclaim_parser_rejects_invalid_or_overlapping_state() {
    for reply in [
        Value::Array(vec![bulk("0-0")]),
        Value::Array(vec![bulk("0-0"), Value::Array(vec![Value::Nil])]),
        Value::Array(vec![
            bulk("0-0"),
            Value::Array(vec![entry("1-0", vec![bulk("event"), bulk("{}")])]),
            Value::Array(vec![bulk("1-0")]),
        ]),
        Value::Array(vec![
            bulk("0-0"),
            Value::Array(Vec::new()),
            Value::Array(vec![bulk("1-0"), bulk("1-0")]),
        ]),
    ] {
        assert!(parse_auto_claim_reply(reply, 10).is_err());
    }

    let oversized = Value::Array(vec![
        bulk("0-0"),
        Value::Array(vec![
            entry("1-0", vec![bulk("event"), bulk("{}")]),
            entry("2-0", vec![bulk("event"), bulk("{}")]),
        ]),
        Value::Array(Vec::new()),
    ]);
    assert!(parse_auto_claim_reply(oversized, 1).is_err());

    let too_many_deleted = Value::Array(vec![
        bulk("0-0"),
        Value::Array(Vec::new()),
        Value::Array((1..=11).map(|id| bulk(&format!("{id}-0"))).collect()),
    ]);
    assert!(parse_auto_claim_reply(too_many_deleted, 1).is_err());
}

#[test]
fn stream_ids_are_unsigned_decimal_pairs_only() {
    assert_eq!(parse_stream_id("0-0").unwrap(), (0, 0));
    assert_eq!(parse_stream_id("42-7").unwrap(), (42, 7));
    for id in ["", "1", "-1", "1-", "-1-0", "1-a", "1-2-3"] {
        assert!(parse_stream_id(id).is_err(), "{id:?} must be rejected");
    }
}
