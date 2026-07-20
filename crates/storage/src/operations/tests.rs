use super::*;

#[test]
fn timestamp_cursor_round_trips_and_rejects_non_v7_ids() {
    let cursor = TimestampCursor {
        at: Utc::now(),
        id: Uuid::now_v7(),
    };
    assert_eq!(TimestampCursor::parse(&cursor.encode()).unwrap(), cursor);
    let invalid = TimestampCursor {
        at: cursor.at,
        id: Uuid::nil(),
    };
    assert!(matches!(
        TimestampCursor::parse(&invalid.encode()),
        Err(OperationsError::InvalidCursor)
    ));
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
        output_per_million: None,
        unit_price: None,
        currency: "USD".to_owned(),
    };
    assert!(matches!(
        validate_prices(&[price.clone(), price]),
        Err(OperationsError::Invalid(message))
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
