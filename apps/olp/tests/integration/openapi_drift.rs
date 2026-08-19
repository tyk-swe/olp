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
fn all_mutation_request_dto_schemas_are_closed() {
    let document = serde_json::to_value(document()).unwrap();
    let schemas = document["components"]["schemas"].as_object().unwrap();
    let paths = document["paths"].as_object().unwrap();

    let mut visited_schemas = std::collections::HashSet::new();
    let mut mutation_request_count = 0;

    for (path_name, path_item) in paths {
        let path_obj = path_item.as_object().unwrap();
        for method in ["post", "put", "patch", "delete"] {
            if let Some(operation) = path_obj.get(method)
                && let Some(request_body) = operation.get("requestBody")
                && let Some(content) = request_body.get("content")
                && let Some(json_content) = content.get("application/json")
                && let Some(schema) = json_content.get("schema")
            {
                mutation_request_count += 1;
                assert_closed_schema(
                    schema,
                    schemas,
                    &format!("{method} {path_name}"),
                    &mut visited_schemas,
                );
            }
        }
    }

    assert!(
        mutation_request_count > 0,
        "mutation operations must be discovered"
    );
}

fn assert_closed_schema(
    schema: &serde_json::Value,
    all_schemas: &serde_json::Map<String, serde_json::Value>,
    context: &str,
    visited: &mut std::collections::HashSet<String>,
) {
    if let Some(ref_path) = schema.get("$ref").and_then(|r| r.as_str()) {
        let schema_name = ref_path.trim_start_matches("#/components/schemas/");
        if !visited.insert(schema_name.to_owned()) {
            return;
        }
        let resolved = all_schemas.get(schema_name).unwrap_or_else(|| {
            panic!("Schema {schema_name} referenced in {context} not found in components.schemas");
        });
        assert_closed_schema(
            resolved,
            all_schemas,
            &format!("{context} -> {schema_name}"),
            visited,
        );
        return;
    }

    for combinator in ["oneOf", "anyOf", "allOf"] {
        if let Some(variants) = schema.get(combinator).and_then(|v| v.as_array()) {
            for (idx, variant) in variants.iter().enumerate() {
                assert_closed_schema(
                    variant,
                    all_schemas,
                    &format!("{context} -> {combinator}[{idx}]"),
                    visited,
                );
            }
        }
    }

    if let Some(items) = schema.get("items") {
        assert_closed_schema(items, all_schemas, &format!("{context} -> items"), visited);
    }

    if let Some(properties) = schema.get("properties").and_then(|p| p.as_object()) {
        let is_object = schema.get("type").is_none_or(|t| {
            t == "object"
                || t.as_array()
                    .is_some_and(|arr| arr.iter().any(|item| item == "object"))
        });
        if is_object {
            assert_eq!(
                schema.get("additionalProperties"),
                Some(&serde_json::Value::Bool(false)),
                "Schema at {context} must have additionalProperties: false"
            );
        }

        for (prop_name, prop_schema) in properties {
            let is_dynamic_value = prop_schema.as_object().is_some_and(|obj| obj.is_empty())
                || (prop_schema.get("type").is_none()
                    && prop_schema.get("$ref").is_none()
                    && prop_schema.get("properties").is_none());
            if !is_dynamic_value {
                assert_closed_schema(
                    prop_schema,
                    all_schemas,
                    &format!("{context}.{prop_name}"),
                    visited,
                );
            }
        }
    }
}
