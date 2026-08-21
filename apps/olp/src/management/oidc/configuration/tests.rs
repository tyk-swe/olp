use axum::{body::Body, http::header};
use http_body_util::BodyExt as _;
use jsonwebtoken::jwk::JwkSet;
use olp_db::{oidc::types::OidcConfiguration, security::envelope::EncryptedSecret};
use serde_json::json;

use super::*;

fn request() -> OidcConfigurationRequest {
    OidcConfigurationRequest {
        discovery_url: "https://idp.example/.well-known/openid-configuration".to_owned(),
        issuer: "https://idp.example".to_owned(),
        client_id: "olp-console".to_owned(),
        client_secret: None,
        enabled: true,
        scopes: default_scopes(),
        email_claim: default_email_claim(),
        groups_claim: default_groups_claim(),
        default_role: None,
        email_role_mappings: Vec::new(),
        group_role_mappings: Vec::new(),
    }
}

fn discovery(auth_methods: &[&str]) -> DiscoveryDocument {
    DiscoveryDocument {
        issuer: "https://idp.example".to_owned(),
        authorization_endpoint: "https://idp.example/authorize".to_owned(),
        token_endpoint: "https://idp.example/token".to_owned(),
        jwks_uri: "https://idp.example/jwks".to_owned(),
        response_types_supported: vec!["code".to_owned()],
        code_challenge_methods_supported: vec!["S256".to_owned()],
        token_endpoint_auth_methods_supported: auth_methods
            .iter()
            .map(|value| (*value).to_owned())
            .collect(),
        id_token_signing_alg_values_supported: vec!["EdDSA".to_owned()],
    }
}

#[test]
fn issuer_policy_rejects_non_origin_components_and_insecure_production_urls() {
    for (value, allow_insecure, accepted) in [
        ("https://idp.example", false, true),
        ("https://idp.example/tenant", false, true),
        ("http://127.0.0.1:8080", true, true),
        ("http://idp.example", false, false),
        ("ftp://idp.example", true, false),
        ("https://user@idp.example", false, false),
        ("https://idp.example?tenant=one", false, false),
        ("https://idp.example#fragment", false, false),
        ("not a URL", false, false),
    ] {
        assert_eq!(
            validate_issuer(value, allow_insecure).is_ok(),
            accepted,
            "unexpected issuer result for {value}"
        );
    }
}

#[test]
fn token_auth_method_uses_interoperable_preference_order() {
    for (methods, expected) in [
        (vec![], Ok("client_secret_basic")),
        (vec!["client_secret_post"], Ok("client_secret_post")),
        (
            vec!["client_secret_post", "client_secret_basic"],
            Ok("client_secret_basic"),
        ),
        (vec!["private_key_jwt"], Err(422_u16)),
    ] {
        let result = choose_token_auth_method(&discovery(&methods));
        match expected {
            Ok(method) => assert_eq!(result.unwrap(), method),
            Err(status) => assert_eq!(result.unwrap_err().status, status),
        }
    }
}

#[test]
fn scopes_are_trimmed_deduplicated_sorted_and_require_openid() {
    assert_eq!(
        normalized_scopes(&[
            " profile ".to_owned(),
            "openid".to_owned(),
            "email".to_owned(),
            "email".to_owned(),
        ])
        .unwrap(),
        ["email", "openid", "profile"]
    );

    for scopes in [
        vec![],
        vec!["email".to_owned()],
        vec!["openid".to_owned(), "two words".to_owned()],
        vec!["openid".to_owned(), "x".repeat(129)],
        std::iter::once("openid".to_owned())
            .chain((0..20).map(|index| format!("scope-{index}")))
            .collect(),
    ] {
        assert_eq!(normalized_scopes(&scopes).unwrap_err().status, 422);
    }
}

#[test]
fn request_shape_validation_bounds_each_untrusted_collection_and_identifier() {
    validate_configuration_request(&request()).unwrap();

    let mut invalid = request();
    invalid.discovery_url = "x".repeat(2_049);
    assert!(validate_configuration_request(&invalid).is_err());

    for client_id in [String::new(), "x".repeat(513), "bad\nclient".to_owned()] {
        let mut invalid = request();
        invalid.client_id = client_id;
        assert!(validate_configuration_request(&invalid).is_err());
    }

    for claim in [String::new(), "contains/slash".to_owned(), "x".repeat(129)] {
        let mut invalid = request();
        invalid.email_claim = claim;
        assert!(validate_configuration_request(&invalid).is_err());
    }

    let mut invalid = request();
    invalid.email_role_mappings = (0..501)
        .map(|index| OidcRoleMappingRequest {
            claim_value: format!("person-{index}"),
            role: "viewer".to_owned(),
        })
        .collect();
    assert!(validate_configuration_request(&invalid).is_err());
}

#[test]
fn role_mappings_trim_values_and_reject_ambiguous_input() {
    let mapping = parse_mapping(&OidcRoleMappingRequest {
        claim_value: " engineering ".to_owned(),
        role: "operator".to_owned(),
    })
    .unwrap();
    assert_eq!(mapping.claim_value, "engineering");
    assert_eq!(mapping.role, Role::Operator);

    for (claim_value, role) in [
        (" ".to_owned(), "viewer"),
        ("x".repeat(257), "viewer"),
        ("bad\nvalue".to_owned(), "viewer"),
        ("engineering".to_owned(), "administrator"),
    ] {
        assert!(
            parse_mapping(&OidcRoleMappingRequest {
                claim_value,
                role: role.to_owned(),
            })
            .is_err()
        );
    }
}

#[test]
fn jwks_requires_a_supported_asymmetric_signing_key() {
    let valid: JwkSet = serde_json::from_value(json!({"keys": [{
        "kty": "OKP", "crv": "Ed25519", "use": "sig", "alg": "EdDSA",
        "kid": "valid", "x": "WOts4ZqTyrsFm_sqwXTJZQngsj3-LQRk-4kz9WFJaYc"
    }]}))
    .unwrap();
    validate_jwks(&valid).unwrap();

    for value in [
        json!({"keys": []}),
        json!({"keys": [{"kty": "oct", "k": "c2VjcmV0", "alg": "HS256"}]}),
        json!({"keys": [{
            "kty": "OKP", "crv": "Ed25519", "use": "enc", "alg": "EdDSA",
            "x": "WOts4ZqTyrsFm_sqwXTJZQngsj3-LQRk-4kz9WFJaYc"
        }]}),
    ] {
        let jwks: JwkSet = serde_json::from_value(value).unwrap();
        assert_eq!(validate_jwks(&jwks).unwrap_err().status, 422);
    }
}

#[tokio::test]
async fn configuration_response_is_redacted_uncacheable_and_versioned() {
    let id = Uuid::now_v7();
    let etag = Uuid::now_v7();
    let now = chrono::Utc::now();
    let secret_nonce = [0xa5; 12];
    let secret_ciphertext = b"encrypted-secret-sentinel".to_vec();
    let encoded_nonce = serde_json::to_string(&secret_nonce).unwrap();
    let encoded_ciphertext = serde_json::to_string(&secret_ciphertext).unwrap();
    let response = configuration_response(OidcConfiguration {
        id,
        discovery_url: "https://idp.example/.well-known/openid-configuration".to_owned(),
        issuer: "https://idp.example".to_owned(),
        authorization_endpoint: "https://idp.example/authorize".to_owned(),
        token_endpoint: "https://idp.example/token".to_owned(),
        jwks_uri: "https://idp.example/jwks".to_owned(),
        token_endpoint_auth_method: "client_secret_basic".to_owned(),
        client_id: "olp-console".to_owned(),
        encrypted_client_secret: EncryptedSecret {
            key_version: 1,
            nonce: secret_nonce,
            ciphertext: secret_ciphertext,
        },
        scopes: default_scopes(),
        email_claim: default_email_claim(),
        groups_claim: default_groups_claim(),
        default_role: Some(Role::Viewer),
        email_role_mappings: vec![OidcRoleMapping {
            claim_value: "owner@example.test".to_owned(),
            role: Role::Owner,
        }],
        group_role_mappings: Vec::new(),
        enabled: true,
        etag,
        created_at: now,
        updated_at: now,
    })
    .unwrap();

    assert_eq!(response.headers()[header::ETAG], format!("\"{etag}\""));
    assert_eq!(response.headers()[header::CACHE_CONTROL], "no-store");
    let body = Body::new(response.into_body())
        .collect()
        .await
        .unwrap()
        .to_bytes();
    let encoded_body = std::str::from_utf8(&body).unwrap();
    for secret in [
        "encrypted_client_secret",
        "ciphertext",
        "nonce",
        &encoded_nonce,
        &encoded_ciphertext,
    ] {
        assert!(
            !encoded_body.contains(secret),
            "leaked secret material: {secret}"
        );
    }
    let body: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(body["id"], id.to_string());
    assert_eq!(body["default_role"], "viewer");
    assert_eq!(body["email_role_mappings"][0]["role"], "owner");
    assert_eq!(body["has_client_secret"], true);
    assert!(body.get("client_secret").is_none());
}
