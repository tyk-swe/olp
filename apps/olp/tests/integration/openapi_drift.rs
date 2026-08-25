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
