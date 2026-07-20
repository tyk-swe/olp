use axum::http::StatusCode;
use olp_providers::{CapabilityCertificationEvidence, CompatibleCapabilityCertificationError};
use olp_storage::{CapabilityRecord, ConfigurationError};
use utoipa::OpenApi;

use crate::management_api::WriteOnlySecret;

use super::{
    ConfigurationApiDoc,
    common::{PageQuery, map_configuration_resource, page},
    providers::certification_item,
};

#[test]
fn write_only_secret_debug_is_redacted() {
    assert_eq!(
        format!("{:?}", WriteOnlySecret::new("top-secret".to_owned())),
        "WriteOnlySecret([REDACTED])"
    );
}

#[test]
fn cursor_and_page_size_are_strict() {
    assert!(
        page(PageQuery {
            cursor: Some("bad".to_owned()),
            limit: None
        })
        .is_err()
    );
    assert!(
        page(PageQuery {
            cursor: None,
            limit: Some(0)
        })
        .is_err()
    );
    assert_eq!(
        page(PageQuery {
            cursor: None,
            limit: Some(100)
        })
        .unwrap(),
        (None, 100)
    );
}

#[test]
fn compatible_certification_contract_requires_etag_and_reports_evidence() {
    let document = serde_json::to_value(ConfigurationApiDoc::openapi()).unwrap();
    let action =
        &document["paths"]["/api/v1/providers/{provider_id}/models/{model_id}/certify"]["post"];
    assert!(
        action["parameters"]
            .as_array()
            .unwrap()
            .iter()
            .any(|parameter| {
                parameter["name"] == "If-Match"
                    && parameter["in"] == "header"
                    && parameter["required"] == true
            })
    );
    assert!(action["responses"].get("200").is_some());
    assert!(action["responses"].get("412").is_some());

    let item = certification_item(
        CapabilityRecord {
            operation: "image_generation".parse().unwrap(),
            surface: "openai".parse().unwrap(),
            mode: "unary".parse().unwrap(),
            source: olp_domain::CapabilitySource::Declared,
            certified_at: None,
        },
        Err(CompatibleCapabilityCertificationError::Unsupported),
    );
    assert!(!item.succeeded);
    assert_eq!(
        item.error_code.as_deref(),
        Some("unsafe_or_unsupported_probe")
    );

    let native = certification_item(
        CapabilityRecord {
            operation: "image_generation".parse().unwrap(),
            surface: "openai".parse().unwrap(),
            mode: "streaming".parse().unwrap(),
            source: olp_domain::CapabilitySource::Declared,
            certified_at: None,
        },
        Ok(CapabilityCertificationEvidence::NativeOpenAiModelDiscoveryAndConnectorContract),
    );
    assert!(native.succeeded);
    assert!(native.error_code.is_none());
    assert!(native.detail.contains("exact provider model"));
    assert!(native.detail.contains("closed native connector contract"));
}

#[test]
fn provider_probe_is_connectivity_only_and_etag_bound() {
    let document = serde_json::to_value(ConfigurationApiDoc::openapi()).unwrap();
    let action = &document["paths"]["/api/v1/providers/{provider_id}/probe"]["post"];
    assert!(action.get("requestBody").is_none());
    assert!(
        action["parameters"]
            .as_array()
            .unwrap()
            .iter()
            .any(|parameter| {
                parameter["name"] == "If-Match"
                    && parameter["in"] == "header"
                    && parameter["required"] == true
            })
    );
    assert!(action["responses"].get("412").is_some());
}

#[test]
fn provider_revision_restore_contract_never_exposes_or_restores_credentials() {
    let document = serde_json::to_value(ConfigurationApiDoc::openapi()).unwrap();
    let properties = document["components"]["schemas"]["ProviderRevisionResponse"]["properties"]
        .as_object()
        .unwrap();
    assert!(properties.contains_key("historical_credential_version"));
    assert!(!properties.contains_key("credential_version_id"));
    assert!(!properties.contains_key("credential"));
    assert!(!properties.contains_key("secret"));

    let action = &document["paths"]["/api/v1/providers/{provider_id}/revisions/{revision_id}/restore-as-draft"]
        ["post"];
    let parameters = action["parameters"].as_array().unwrap();
    for required_header in ["If-Match", "Idempotency-Key"] {
        assert!(parameters.iter().any(|parameter| {
            parameter["name"] == required_header
                && parameter["in"] == "header"
                && parameter["required"] == true
        }));
    }
    assert!(action["responses"].get("412").is_some());
}

#[test]
fn provider_and_revision_model_inventories_are_bounded_pages() {
    let document = serde_json::to_value(ConfigurationApiDoc::openapi()).unwrap();
    for schema in [
        "ProviderSummaryResponse",
        "ProviderDetailResponse",
        "ProviderRevisionSummaryResponse",
        "ProviderRevisionResponse",
    ] {
        let properties = document["components"]["schemas"][schema]["properties"]
            .as_object()
            .unwrap();
        assert!(!properties.contains_key("models"));
        assert!(properties.contains_key("model_count"));
        assert!(properties.contains_key("enabled_model_count"));
        assert!(properties.contains_key("capability_count"));
        assert!(properties.contains_key("certified_capability_count"));
    }
    for path in [
        "/api/v1/provider-models",
        "/api/v1/providers/{provider_id}/models",
        "/api/v1/providers/{provider_id}/revisions/{revision_id}/models",
    ] {
        let action = &document["paths"][path]["get"];
        assert!(action["responses"].get("200").is_some());
        let parameters = action["parameters"].as_array().unwrap();
        assert!(
            parameters
                .iter()
                .any(|parameter| parameter["name"] == "cursor")
        );
        assert!(
            parameters
                .iter()
                .any(|parameter| parameter["name"] == "limit")
        );
    }
    let inventory_parameters = document["paths"]["/api/v1/provider-models"]["get"]["parameters"]
        .as_array()
        .unwrap();
    assert!(
        inventory_parameters
            .iter()
            .any(|parameter| parameter["name"] == "enabled")
    );
}

#[test]
fn provider_revision_diff_contract_documents_hard_response_ceilings() {
    let document = serde_json::to_value(ConfigurationApiDoc::openapi()).unwrap();
    let action = &document["paths"]["/api/v1/providers/{provider_id}/revisions/diff"]["get"];
    assert!(action["responses"].get("422").is_some());

    let properties =
        &document["components"]["schemas"]["ProviderRevisionDiffResponse"]["properties"];
    for field in ["models_added", "models_removed", "models_changed"] {
        assert_eq!(properties[field]["maxItems"], 2_000);
    }
    for field in ["capabilities_added", "capabilities_removed"] {
        assert_eq!(properties[field]["maxItems"], 32_000);
    }

    let problem = map_configuration_resource(ConfigurationError::ProviderRevisionDiffTooLarge {
        dimension: "models",
        maximum: 2_000,
    });
    assert_eq!(problem.status, StatusCode::UNPROCESSABLE_ENTITY.as_u16());
    assert_eq!(
        problem.errors["revisions"],
        ["provider revision diff supports at most 2000 models per revision"]
    );
}
