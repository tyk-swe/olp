use super::*;

#[test]
fn strong_etag_parser_rejects_wildcards_and_unquoted_values() {
    let id = Uuid::now_v7();
    let mut headers = HeaderMap::new();
    headers.insert(
        header::IF_MATCH,
        HeaderValue::from_str(&format!("\"{id}\"")).unwrap(),
    );
    assert_eq!(if_match(&headers).unwrap(), id);
    headers.insert(
        header::IF_MATCH,
        HeaderValue::from_str(&id.to_string()).unwrap(),
    );
    assert_eq!(if_match(&headers).unwrap_err().status, 400);
    headers.insert(header::IF_MATCH, HeaderValue::from_static("*"));
    assert_eq!(if_match(&headers).unwrap_err().status, 400);
}

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
fn media_job_surface_preserves_wire_contract() {
    assert_eq!(media_job_surface_wire_value(Surface::OpenAi), "openai");
    assert_eq!(
        media_job_surface_wire_value(Surface::Anthropic),
        "anthropic"
    );
    assert_eq!(media_job_surface_wire_value(Surface::Gemini), "gemini");
}

#[test]
fn audit_contract_omits_unavailable_request_provenance() {
    let document = serde_json::to_value(OperationsApiDoc::openapi()).unwrap();
    let properties = document["components"]["schemas"]["AuditEventResponse"]["properties"]
        .as_object()
        .unwrap();
    assert!(!properties.contains_key("source_ip"));
    assert!(!properties.contains_key("user_agent_family"));
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
fn usage_contract_exposes_unclean_epoch_uncertainty() {
    let document = serde_json::to_value(OperationsApiDoc::openapi()).unwrap();
    for schema in ["UsageSummaryResponse", "UsageCompletenessResponse"] {
        let properties = document["components"]["schemas"][schema]["properties"]
            .as_object()
            .unwrap();
        assert!(properties.contains_key("ingestion_gap_events"));
        assert!(properties.contains_key("uncertain_gap_count"));
    }
}
