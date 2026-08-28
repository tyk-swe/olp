use olp::management::openapi::document;

#[test]
fn checked_in_management_schema_matches_generated_contract() {
    let generated = serde_json::to_value(document()).unwrap();
    let checked_in: serde_json::Value =
        serde_json::from_str(include_str!("../../../../openapi/management.json")).unwrap();
    assert_eq!(
        generated, checked_in,
        "run the OpenAPI export before committing API changes"
    );
}

#[test]
fn v1_list_contracts_keep_data_alongside_items() {
    let document = serde_json::to_value(document()).unwrap();
    for schema in [
        "AuditListResponse",
        "InvitationListResponse",
        "MediaJobListResponse",
        "OidcIdentityListResponse",
        "PricingRevisionsResponse",
        "ProviderHealthResponse",
        "RequestListResponse",
        "RequestMetadataGatewayEpochListResponse",
        "RuntimeGenerationListResponse",
        "SessionListResponse",
        "SettingsResponse",
        "UsageBreakdownResponse",
        "UsageTimeSeriesResponse",
        "UserListResponse",
    ] {
        let response_schema = &document["components"]["schemas"][schema];
        let properties = response_schema["properties"].as_object().unwrap();
        assert_eq!(properties["data"], properties["items"], "{schema}");
        let required = response_schema["required"].as_array().unwrap();
        assert!(required.iter().any(|field| field == "data"), "{schema}");
        assert!(required.iter().any(|field| field == "items"), "{schema}");
    }
}

/// Every mutation that demands `Idempotency-Key` can reject the header (400)
/// and can refuse a reused key (409). A generated client that does not know
/// those exist treats them as transport failures.
#[test]
fn idempotent_operations_declare_their_header_and_conflict_responses() {
    let document = serde_json::to_value(document()).unwrap();
    let paths = document["paths"].as_object().unwrap();
    let mut checked = 0;
    for (path, methods) in paths {
        for (method, operation) in methods.as_object().unwrap() {
            let demands_key = operation["parameters"]
                .as_array()
                .is_some_and(|parameters| {
                    parameters.iter().any(|parameter| {
                        parameter["in"] == "header" && parameter["name"] == "Idempotency-Key"
                    })
                });
            if !demands_key {
                continue;
            }
            checked += 1;
            let responses = operation["responses"].as_object().unwrap();
            for status in ["400", "409"] {
                assert!(
                    responses.contains_key(status),
                    "{method} {path} requires Idempotency-Key but does not declare {status}"
                );
            }
        }
    }
    assert!(
        checked >= 12,
        "expected the idempotent mutations to be found"
    );
}

/// Every paginated operation that accepts a `limit` query parameter can reject
/// an out-of-range page size (400). A generated client that does not know
/// this exists treats the rejection as a transport failure.
#[test]
fn paginated_operations_declare_the_invalid_page_size_response() {
    let document = serde_json::to_value(document()).unwrap();
    let paths = document["paths"].as_object().unwrap();
    let mut checked = 0;
    for (path, methods) in paths {
        for (method, operation) in methods.as_object().unwrap() {
            let accepts_limit = operation["parameters"]
                .as_array()
                .is_some_and(|parameters| {
                    parameters
                        .iter()
                        .any(|parameter| parameter["in"] == "query" && parameter["name"] == "limit")
                });
            if !accepts_limit {
                continue;
            }
            checked += 1;
            let responses = operation["responses"].as_object().unwrap();
            assert!(
                responses.contains_key("400"),
                "{method} {path} accepts limit query parameter but does not declare 400"
            );
        }
    }
    assert!(
        checked >= 20,
        "expected the paginated operations to be found"
    );
}
