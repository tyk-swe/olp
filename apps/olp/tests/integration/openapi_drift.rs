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
