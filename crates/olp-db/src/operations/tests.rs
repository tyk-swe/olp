use chrono::{DateTime, Utc};
use olp_engine::domain::{
    canonical::identity::OperationKind, provider::ProviderState, routing::provider::ProviderKind,
};
use uuid::Uuid;

use super::cursor::{
    Error, Timestamp, checked_u16, checked_u64, optional_i32_u64, optional_u16, optional_u64,
    trimmed_optional,
};
use super::{
    health::provider_health_status,
    pricing::{PriceInput, validate_decimal, validate_prices},
};
use crate::usage::query::{ceil_usage_hour, floor_usage_hour};

fn assert_invalid<T: std::fmt::Debug>(result: Result<T, Error>, expected: &str) {
    match result {
        Err(Error::Invalid(message)) => assert_eq!(message, expected),
        other => panic!("unexpected result: {other:?}"),
    }
}

#[test]
fn timestamp_cursor_round_trips_and_rejects_malformed_or_non_v7_values() {
    let cursor = Timestamp {
        at: "2025-02-03T04:05:06Z".parse().unwrap(),
        id: Uuid::now_v7(),
    };
    assert_eq!(Timestamp::parse(&cursor.encode()).unwrap(), cursor);
    for invalid in [
        "not-base64".to_owned(),
        "bm90IGpzb24".to_owned(),
        Timestamp {
            at: cursor.at,
            id: Uuid::nil(),
        }
        .encode(),
    ] {
        assert!(
            matches!(Timestamp::parse(&invalid), Err(Error::InvalidCursor)),
            "accepted {invalid}"
        );
    }
}

#[test]
fn stored_number_conversions_cover_boundaries_and_name_invalid_fields() {
    assert_eq!(optional_u16(Some(65_535), "value").unwrap(), Some(u16::MAX));
    assert_eq!(checked_u16(i16::MAX, "value").unwrap(), 32_767);
    assert_eq!(
        optional_i32_u64(Some(i32::MAX), "value").unwrap(),
        Some(i32::MAX as u64)
    );
    assert_eq!(checked_u64(i64::MAX, "value").unwrap(), i64::MAX as u64);

    for absent in [
        optional_u16(None, "value").map(|value| value.map(u64::from)),
        optional_u64(None, "value"),
        optional_i32_u64(None, "value"),
    ] {
        assert_eq!(absent.unwrap(), None);
    }
    for invalid in [
        checked_u16(-1, "value").map(u64::from),
        checked_u64(-1, "value"),
    ] {
        assert_invalid(invalid, "stored value is invalid");
    }
    for invalid in [
        optional_u16(Some(65_536), "value").map(|value| value.map(u64::from)),
        optional_u64(Some(-1), "value"),
        optional_i32_u64(Some(-1), "value"),
    ] {
        assert_invalid(invalid, "stored value is invalid");
    }
}

#[test]
fn optional_text_is_trimmed_without_changing_presence() {
    for (input, expected) in [
        (None, None),
        (Some("  model-a\n"), Some("model-a")),
        (Some(" \t"), Some("")),
    ] {
        assert_eq!(
            trimmed_optional(input.map(str::to_owned)),
            expected.map(str::to_owned)
        );
    }
}

#[test]
fn validates_exact_non_negative_decimal_prices() {
    for valid in ["0", "0.000001", "123456789012.123456789012"] {
        validate_decimal(valid).unwrap();
    }
    for invalid in ["", "-1", ".1", "1.", "1e3", "1.0000000000001"] {
        assert!(validate_decimal(invalid).is_err(), "accepted {invalid}");
    }
}

#[test]
fn rejects_duplicate_pricing_dimensions_within_a_scope() {
    let price = PriceInput {
        provider_kind: ProviderKind::OpenAi,
        provider_id: None,
        model: "model".to_owned(),
        operation: OperationKind::Generation,
        input_per_million: Some("1".to_owned()),
        cached_input_per_million: None,
        output_per_million: None,
        unit_price: None,
        currency: "USD".to_owned(),
    };
    assert!(matches!(
        validate_prices(&[price.clone(), price]),
        Err(Error::Invalid(message))
            if message.contains("duplicate scoped dimensions")
    ));
}

#[test]
fn accepts_unit_only_media_pricing() {
    validate_prices(&[PriceInput {
        provider_kind: ProviderKind::OpenAi,
        provider_id: None,
        model: "image-model".to_owned(),
        operation: OperationKind::ImageGeneration,
        input_per_million: None,
        cached_input_per_million: None,
        output_per_million: None,
        unit_price: Some("0.04".to_owned()),
        currency: "USD".to_owned(),
    }])
    .unwrap();
}

#[test]
fn rejects_noncanonical_pricing_dimensions() {
    assert!("open_ai".parse::<ProviderKind>().is_err());
    assert!("chat".parse::<OperationKind>().is_err());
}

#[test]
fn retained_hour_boundaries_are_never_rounded_down() {
    let exact = "2026-07-12T10:00:00Z".parse::<DateTime<Utc>>().unwrap();
    let partial = "2026-07-12T10:15:30Z".parse::<DateTime<Utc>>().unwrap();
    assert_eq!(floor_usage_hour(partial), exact);
    assert_eq!(ceil_usage_hour(exact), exact);
    assert_eq!(
        ceil_usage_hour(partial),
        "2026-07-12T11:00:00Z".parse::<DateTime<Utc>>().unwrap()
    );
}

#[test]
fn provider_health_prioritizes_latest_probe_and_error_ratio() {
    let now = Utc::now();
    assert_eq!(
        provider_health_status(ProviderState::Disabled, None, None, None, 0, 0),
        "disabled"
    );
    assert_eq!(
        provider_health_status(ProviderState::Active, Some(now), Some("failed"), None, 0, 0,),
        "unavailable"
    );
    assert_eq!(
        provider_health_status(ProviderState::Active, None, None, Some(now), 100, 95),
        "healthy"
    );
    assert_eq!(
        provider_health_status(ProviderState::Active, None, None, Some(now), 100, 89),
        "degraded"
    );
    assert_eq!(
        provider_health_status(ProviderState::Active, None, None, Some(now), 10, 5),
        "unavailable"
    );
}
