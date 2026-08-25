use super::sessions::CSRF_HEADER;
use crate::public_http::request_cookies::SESSION_COOKIE;
use utoipa::OpenApi as _;

#[must_use]
pub fn document() -> serde_json::Value {
    let mut document = super::ApiDoc::openapi();
    document.merge(super::oidc::openapi());
    document.merge(super::operations::OperationsApiDoc::openapi());
    document.merge(super::configuration::ConfigurationApiDoc::openapi());
    document.merge(super::playground::PlaygroundApiDoc::openapi());
    complete_contract(document)
}

fn complete_contract(document: utoipa::openapi::OpenApi) -> serde_json::Value {
    let mut value = serde_json::to_value(document).expect("generated OpenAPI is serializable");
    let components = value
        .get_mut("components")
        .and_then(serde_json::Value::as_object_mut)
        .expect("generated OpenAPI has components");
    components.insert(
        "securitySchemes".to_owned(),
        serde_json::json!({
            "sessionCookie": {
                "type": "apiKey",
                "in": "cookie",
                "name": SESSION_COOKIE,
                "description": "Opaque PostgreSQL-backed management session."
            },
            "csrfToken": {
                "type": "apiKey",
                "in": "header",
                "name": CSRF_HEADER,
                "description": "Double-submit CSRF token required with authenticated mutations."
            },
            "bootstrapSetupToken": {
                "type": "apiKey",
                "in": "header",
                "name": "X-OLP-Setup-Token",
                "description": "One-time bootstrap token required only while creating the first installation owner."
            }
        }),
    );

    let public_operations = [
        ("/api/v1/openapi.json", "get"),
        ("/api/v1/auth/capabilities", "get"),
        ("/api/v1/setup/status", "get"),
        ("/api/v1/setup", "post"),
        ("/api/v1/sessions", "post"),
        ("/api/v1/invitations/accept", "post"),
        ("/api/v1/oidc/login", "get"),
        ("/api/v1/oidc/login", "post"),
        ("/api/v1/oidc/callback", "get"),
    ];
    let paths = value
        .get_mut("paths")
        .and_then(serde_json::Value::as_object_mut)
        .expect("generated OpenAPI has paths");
    for (path, item) in paths {
        let Some(methods) = item.as_object_mut() else {
            continue;
        };
        for (method, operation) in methods {
            if !matches!(method.as_str(), "get" | "post" | "put" | "patch" | "delete") {
                continue;
            }
            let Some(operation) = operation.as_object_mut() else {
                continue;
            };
            let is_public = public_operations
                .iter()
                .any(|(public_path, public_method)| path == public_path && method == public_method);
            let is_bootstrap_setup = path == "/api/v1/setup" && method == "post";
            operation.insert(
                "security".to_owned(),
                if is_bootstrap_setup {
                    serde_json::json!([{ "bootstrapSetupToken": [] }])
                } else if is_public {
                    serde_json::json!([])
                } else if matches!(method.as_str(), "post" | "put" | "patch" | "delete") {
                    serde_json::json!([{ "sessionCookie": [], "csrfToken": [] }])
                } else {
                    serde_json::json!([{ "sessionCookie": [] }])
                },
            );

            let has_if_match = operation
                .get("parameters")
                .and_then(serde_json::Value::as_array)
                .is_some_and(|parameters| {
                    parameters.iter().any(|parameter| {
                        parameter.get("name").and_then(serde_json::Value::as_str)
                            == Some("If-Match")
                    })
                });
            if let Some(responses) = operation
                .get_mut("responses")
                .and_then(serde_json::Value::as_object_mut)
            {
                for (status, response) in responses.iter_mut() {
                    normalize_problem_content(response);
                    if has_if_match && status.starts_with('2') {
                        response
                            .as_object_mut()
                            .expect("OpenAPI response is an object")
                            .entry("headers")
                            .or_insert_with(|| serde_json::json!({}))
                            .as_object_mut()
                            .expect("OpenAPI response headers are an object")
                            .insert(
                                "ETag".to_owned(),
                                serde_json::json!({
                                    "description": "Current strong entity tag.",
                                    "schema": { "type": "string" }
                                }),
                            );
                    }
                }
                if !is_public {
                    responses
                        .entry("401")
                        .or_insert_with(|| problem_response("Authentication required."));
                    responses.entry("403").or_insert_with(|| {
                        problem_response(
                            "The session lacks permission or mutation CSRF/origin checks failed.",
                        )
                    });
                }
                responses
                    .entry("500")
                    .or_insert_with(|| problem_response("The request could not be completed."));
            }
        }
    }
    // Utoipa's typed OpenAPI model is intentionally narrower than OpenAPI
    // 3.1 in a few extension points (notably response-header schemas). The
    // generated contract is the JSON document served and drift-checked by OLP,
    // so retain the standards-compliant transformed value instead of trying to
    // deserialize it back through that lossy model.
    value
}

fn normalize_problem_content(response: &mut serde_json::Value) {
    let Some(content) = response
        .get_mut("content")
        .and_then(serde_json::Value::as_object_mut)
    else {
        return;
    };
    let is_problem = content.get("application/json").is_some_and(|media| {
        media
            .pointer("/schema/$ref")
            .and_then(serde_json::Value::as_str)
            .is_some_and(|reference| reference.ends_with("/Problem"))
    });
    if is_problem && let Some(media) = content.remove("application/json") {
        content.insert("application/problem+json".to_owned(), media);
    }
}

fn problem_response(description: &str) -> serde_json::Value {
    serde_json::json!({
        "description": description,
        "content": {
            "application/problem+json": {
                "schema": { "$ref": "#/components/schemas/Problem" }
            }
        }
    })
}

#[cfg(test)]
mod tests {
    use super::document;

    #[test]
    fn the_rotated_secret_survives_client_generation() {
        let document = document();
        let secret = document
            .pointer("/components/schemas/RotateApiKeyResponse/properties/secret")
            .expect("the rotation response declares its secret");
        assert!(
            secret.get("writeOnly").is_none(),
            "a writeOnly property is dropped from generated response models, \
             so the once-shown secret would be unreadable"
        );
        assert!(
            document
                .pointer("/components/schemas/RotateApiKeyResponse/required")
                .and_then(serde_json::Value::as_array)
                .is_some_and(|required| required.iter().any(|field| field == "secret"))
        );
    }

    /// 201s that create nothing addressable: two are authentication
    /// exchanges, one bootstraps the installation itself, and two create
    /// records the API exposes only through their parent collection.
    const CREATES_WITHOUT_A_RESOURCE_URL: [&str; 5] = [
        "/api/v1/sessions",
        "/api/v1/setup",
        "/api/v1/invitations/accept",
        "/api/v1/pricing/revisions",
        "/api/v1/providers/{provider_id}/credentials",
    ];

    #[test]
    fn every_create_that_returns_201_points_at_what_it_created() {
        let document = document();
        let paths = document["paths"].as_object().unwrap();
        let mut checked = 0;
        for (path, methods) in paths {
            let Some(created) = methods.pointer("/post/responses/201") else {
                continue;
            };
            if CREATES_WITHOUT_A_RESOURCE_URL.contains(&path.as_str()) {
                continue;
            }
            checked += 1;
            assert!(
                created.pointer("/headers/Location").is_some(),
                "POST {path} returns 201 without a Location header"
            );
        }
        assert!(checked >= 4, "expected the create endpoints to be found");
    }

    #[test]
    fn every_paginated_collection_declares_the_same_page_bounds() {
        let document = document();
        let paths = document["paths"].as_object().unwrap();
        let mut checked = 0;
        for (path, methods) in paths {
            for (method, operation) in methods.as_object().unwrap() {
                let Some(parameters) = operation["parameters"].as_array() else {
                    continue;
                };
                for parameter in parameters {
                    if parameter["name"] != "limit" {
                        continue;
                    }
                    checked += 1;
                    assert_eq!(
                        parameter
                            .pointer("/schema/maximum")
                            .and_then(serde_json::Value::as_u64),
                        Some(200),
                        "{method} {path} declares a different page-size ceiling"
                    );
                    assert_eq!(
                        parameter
                            .pointer("/schema/minimum")
                            .and_then(serde_json::Value::as_u64),
                        Some(1),
                        "{method} {path} declares a different minimum page size"
                    );
                }
            }
        }
        assert!(
            checked >= 10,
            "expected the paginated collections to be found"
        );
    }
}
