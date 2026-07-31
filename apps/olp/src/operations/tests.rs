use super::*;

#[test]
fn pagination_and_time_ranges_reject_silent_clamping_or_reversal() {
    assert_eq!(page_limit(None).unwrap(), 50);
    assert_eq!(page_limit(Some(200)).unwrap(), 200);
    assert_eq!(page_limit(Some(0)).unwrap_err().status, 422);
    let now = Utc::now();
    assert!(validate_time_range("start", now, "end", now).is_err());
    assert!(validate_time_range("start", now - chrono::Duration::seconds(1), "end", now).is_ok());
}

#[test]
fn pricing_openapi_lists_current_provider_kinds() {
    let document = serde_json::to_value(OperationsApiDoc::openapi()).unwrap();
    assert_eq!(
        document["components"]["schemas"]["ProviderKind"]["enum"],
        serde_json::json!([
            "openai",
            "anthropic",
            "gemini",
            "vertex_ai",
            "bedrock",
            "azure_openai",
            "openai_compatible"
        ])
    );
}

#[test]
fn usage_series_and_breakdown_publish_flat_query_parameters() {
    let document = serde_json::to_value(OperationsApiDoc::openapi()).unwrap();
    for (path, endpoint_parameter) in [
        ("/api/v1/usage/time-series", "granularity"),
        ("/api/v1/usage/breakdown", "dimension"),
    ] {
        let parameters = document["paths"][path]["get"]["parameters"]
            .as_array()
            .unwrap();
        let names = parameters
            .iter()
            .filter_map(|parameter| parameter["name"].as_str())
            .collect::<std::collections::BTreeSet<_>>();
        assert!(names.contains("start"));
        assert!(names.contains("end"));
        assert!(names.contains(endpoint_parameter));
        assert!(!names.contains("usage"));
    }
}

#[test]
fn usage_contract_names_request_metadata_evidence_precisely() {
    let document = serde_json::to_value(OperationsApiDoc::openapi()).unwrap();
    for schema in ["UsageSummaryResponse", "UsageCompletenessResponse"] {
        let properties = document["components"]["schemas"][schema]["properties"]
            .as_object()
            .unwrap();
        assert!(properties.contains_key("request_metadata_gap_events"));
        assert!(properties.contains_key("uncertain_request_metadata_gap_count"));
        assert!(properties.contains_key("request_metadata_consumer"));
    }
}

#[test]
fn request_metadata_gateway_epochs_have_their_own_api_namespace() {
    let document = serde_json::to_value(OperationsApiDoc::openapi()).unwrap();
    assert!(
        document["paths"]
            .get("/api/v1/request-metadata/gateway-epochs")
            .is_some()
    );
    assert!(
        document["paths"]
            .get("/api/v1/usage/gateway-epochs")
            .is_none()
    );
}
