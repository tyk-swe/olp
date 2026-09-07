use crate::access::sessions::CSRF_HEADER;
use crate::http::request_cookies::SESSION_COOKIE;

#[must_use]
pub fn document() -> serde_json::Value {
    let mut document = super::routes().split_for_parts().1;
    document.info.title = "OpenLLMProxy Management API".to_owned();
    document.info.version = "3.0.0".to_owned();
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

    // Utoipa's typed OpenAPI model is intentionally narrower than OpenAPI
    // 3.1 in a few extension points (notably response-header schemas). The
    // generated contract is the JSON document served and drift-checked by OLP,
    // so retain the standards-compliant transformed value instead of trying to
    // deserialize it back through that lossy model.
    value
}

#[cfg(test)]
mod tests {
    use crate::http::control::openapi::document;

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
        "/api/v3/sessions",
        "/api/v3/setup",
        "/api/v3/invitations/accept",
        "/api/v3/pricing/revisions",
        "/api/v3/providers/{provider_id}/credentials",
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
