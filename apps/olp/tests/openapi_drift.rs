use olp::management_openapi;

#[test]
fn checked_in_management_schema_matches_generated_contract() {
    let generated = serde_json::to_value(management_openapi()).unwrap();
    let checked_in: serde_json::Value =
        serde_json::from_str(include_str!("../../../openapi/management.json")).unwrap();
    assert_eq!(
        generated, checked_in,
        "run the OpenAPI export before committing API changes"
    );
}
