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
fn the_openapi_endpoint_documents_itself() {
    let generated = serde_json::to_value(document()).unwrap();
    assert!(
        generated["paths"]
            .as_object()
            .is_some_and(|paths| paths.contains_key("/api/v1/openapi.json")),
        "GET /api/v1/openapi.json must be listed in the document it serves"
    );
}
