use super::*;
use axum::http::{HeaderMap, HeaderValue, StatusCode, header};
use base64::{Engine as _, engine::general_purpose::STANDARD};
use chrono::Utc;
use jsonwebtoken::{Algorithm, EncodingKey, Header, encode, jwk::JwkSet};
use olp_storage::{MasterKey, OidcConfiguration, OidcFlowPurpose};
use serde_json::{Value, json};
use tower::ServiceExt as _;
use uuid::Uuid;
use zeroize::Zeroizing;

use super::callback::matching_login_callback_flow;
use super::claims::{bounded_claim, validate_id_token};
use super::configuration::{OidcSecret, default_email_claim, default_groups_claim, default_scopes};
use super::helpers::optional_if_match;
use super::session::{
    FLOW_TTL, LOGIN_FLOW_COOKIE, LOGIN_FLOW_COOKIE_VERSION, LoginFlowCookiePayload,
    OidcCallbackState, OidcFlowId, consume_login_flow_cookie, flow_cookie_name,
    seal_login_flow_cookie,
};

// Public test fixture used only to exercise the verifier.
const ED25519_PRIVATE_DER_B64: &str =
    "MC4CAQAwBQYDK2VwBCIEIBrf5enAkeYcV99WmDtSpbEHFio5SdSot7TRRtzNDW11";
const ED25519_PUBLIC_X: &str = "WOts4ZqTyrsFm_sqwXTJZQngsj3-LQRk-4kz9WFJaYc";

#[test]
fn configuration_debug_redacts_client_secret() {
    let request = OidcConfigurationRequest {
        discovery_url: "https://idp.example/.well-known/openid-configuration".to_owned(),
        issuer: "https://idp.example".to_owned(),
        client_id: "olp".to_owned(),
        client_secret: Some(OidcSecret(Zeroizing::new("super-secret".to_owned()))),
        enabled: true,
        scopes: default_scopes(),
        email_claim: default_email_claim(),
        groups_claim: default_groups_claim(),
        default_role: None,
        email_role_mappings: vec![],
        group_role_mappings: vec![],
    };
    assert!(!format!("{request:?}").contains("super-secret"));
}

#[test]
fn hmac_id_tokens_are_rejected_before_key_use() {
    let configuration = test_configuration();
    let header = Header::new(Algorithm::HS256);
    let token = encode(
        &header,
        &json!({
            "iss": configuration.issuer,
            "sub": "subject",
            "aud": configuration.client_id,
            "exp": Utc::now().timestamp() + 300,
            "nonce": "nonce"
        }),
        &EncodingKey::from_secret(b"secret"),
    )
    .unwrap();
    assert!(
        validate_id_token(
            &token,
            &JwkSet { keys: vec![] },
            &configuration,
            "nonce",
            false
        )
        .is_err()
    );
}

#[test]
fn malformed_claims_are_rejected_without_panicking() {
    let claims = json!({"sub": ["not", "a", "string"]});
    assert!(bounded_claim(&claims, "sub", 255).is_err());
}

#[test]
fn optional_etag_parser_requires_a_strong_quoted_uuid() {
    let id = Uuid::now_v7();
    let mut headers = HeaderMap::new();
    assert_eq!(optional_if_match(&headers).unwrap(), None);
    headers.insert(
        header::IF_MATCH,
        HeaderValue::from_str(&format!("\"{id}\"")).unwrap(),
    );
    assert_eq!(optional_if_match(&headers).unwrap(), Some(id));
    headers.insert(
        header::IF_MATCH,
        HeaderValue::from_str(&id.to_string()).unwrap(),
    );
    assert_eq!(optional_if_match(&headers).unwrap_err().status, 400);
}

#[test]
fn stateless_login_cookie_is_encrypted_origin_bound_and_state_checked() {
    let mut state = ManagementState::new(
        crate::ApiMode::Control,
        None,
        std::sync::Arc::new(crate::RuntimeManager::empty()),
        "https://console.example.test",
        std::path::PathBuf::from("missing-console"),
    );
    let master_key = MasterKey::new(7, [42; 32]);
    state.master_key = Some(std::sync::Arc::new(MasterKey::new(7, [42; 32])));
    let configuration_id = Uuid::now_v7();
    let configuration_etag = Uuid::now_v7();
    let state_token = "a".repeat(43);
    let payload = LoginFlowCookiePayload {
        version: LOGIN_FLOW_COOKIE_VERSION,
        flow_id: Uuid::now_v7(),
        state: state_token.clone(),
        nonce: "b".repeat(43),
        pkce_verifier: "c".repeat(43),
        configuration_id,
        configuration_etag,
        expires_at_unix: (Utc::now() + FLOW_TTL).timestamp(),
        return_to: crate::RelativeReturnTo::parse("/settings?tab=security#sessions").unwrap(),
    };
    let encoded = seal_login_flow_cookie(&state, &master_key, &payload).unwrap();
    assert!(encoded.starts_with("v2."));
    assert!(!encoded.contains(&payload.nonce));

    let callback_state = OidcCallbackState::parse(OidcCallbackState::encode(
        OidcFlowId::from_uuid(payload.flow_id),
        &state_token,
    ))
    .unwrap();
    let flow = consume_login_flow_cookie(&state, &encoded, &callback_state).unwrap();
    assert_eq!(flow.purpose, OidcFlowPurpose::Login);
    assert_eq!(flow.configuration_id, configuration_id);
    assert_eq!(flow.configuration_etag, configuration_etag);
    assert_eq!(flow.return_to.as_str(), "/settings?tab=security#sessions");
    assert_eq!(flow.login_consumption.unwrap().flow_id, payload.flow_id);
    assert_eq!(flow.secret.nonce, payload.nonce);
    let wrong_state = OidcCallbackState::parse(OidcCallbackState::encode(
        OidcFlowId::from_uuid(payload.flow_id),
        &"d".repeat(43),
    ))
    .unwrap();
    assert!(consume_login_flow_cookie(&state, &encoded, &wrong_state).is_err());

    let mut other_origin = ManagementState::new(
        crate::ApiMode::Control,
        None,
        std::sync::Arc::new(crate::RuntimeManager::empty()),
        "https://other.example.test",
        std::path::PathBuf::from("missing-console"),
    );
    other_origin.master_key = Some(std::sync::Arc::new(master_key));
    assert!(consume_login_flow_cookie(&other_origin, &encoded, &callback_state).is_err());
}

#[test]
fn callback_prefers_the_flow_specific_cookie_matching_its_state() {
    let mut state = ManagementState::new(
        crate::ApiMode::Control,
        None,
        std::sync::Arc::new(crate::RuntimeManager::empty()),
        "https://console.example.test",
        std::path::PathBuf::from("missing-console"),
    );
    let master_key = MasterKey::new(1, [8; 32]);
    state.master_key = Some(std::sync::Arc::new(MasterKey::new(1, [8; 32])));
    let login_secret = "a".repeat(43);
    let link_secret = "d".repeat(43);
    let login_id = OidcFlowId::generate();
    let link_id = OidcFlowId::generate();
    let encoded = seal_login_flow_cookie(
        &state,
        &master_key,
        &LoginFlowCookiePayload {
            version: LOGIN_FLOW_COOKIE_VERSION,
            flow_id: login_id.as_uuid(),
            state: login_secret.clone(),
            nonce: "b".repeat(43),
            pkce_verifier: "c".repeat(43),
            configuration_id: Uuid::now_v7(),
            configuration_etag: Uuid::now_v7(),
            expires_at_unix: (Utc::now() + FLOW_TTL).timestamp(),
            return_to: Default::default(),
        },
    )
    .unwrap();
    let login_state = OidcCallbackState::encode(login_id, &login_secret);
    let link_state = OidcCallbackState::encode(link_id, &link_secret);
    let mut headers = HeaderMap::new();
    headers.append(
        header::COOKIE,
        HeaderValue::from_str(&format!(
            "{}={encoded}",
            flow_cookie_name(OidcFlowPurpose::Login, login_id)
        ))
        .unwrap(),
    );
    headers.append(
        header::COOKIE,
        HeaderValue::from_str(&format!(
            "{}={}",
            flow_cookie_name(OidcFlowPurpose::Link, link_id),
            "e".repeat(43)
        ))
        .unwrap(),
    );

    assert!(
        matching_login_callback_flow(&state, &headers, &login_state)
            .unwrap()
            .is_some()
    );
    assert!(
        matching_login_callback_flow(&state, &headers, &link_state)
            .unwrap()
            .is_none()
    );

    // A state for the same flow ID but a different secret selects the exact
    // login cookie and fails its authenticated binding rather than falling
    // through to another tab's flow.
    let wrong_login_state = OidcCallbackState::encode(login_id, &link_secret);
    assert!(matching_login_callback_flow(&state, &headers, &wrong_login_state).is_err());
}

#[test]
fn expired_or_tampered_stateless_login_cookie_is_rejected() {
    let mut state = ManagementState::new(
        crate::ApiMode::Control,
        None,
        std::sync::Arc::new(crate::RuntimeManager::empty()),
        "https://console.example.test",
        std::path::PathBuf::from("missing-console"),
    );
    let master_key = MasterKey::new(1, [7; 32]);
    state.master_key = Some(std::sync::Arc::new(MasterKey::new(1, [7; 32])));
    let payload = LoginFlowCookiePayload {
        version: LOGIN_FLOW_COOKIE_VERSION,
        flow_id: Uuid::now_v7(),
        state: "a".repeat(43),
        nonce: "b".repeat(43),
        pkce_verifier: "c".repeat(43),
        configuration_id: Uuid::now_v7(),
        configuration_etag: Uuid::now_v7(),
        expires_at_unix: Utc::now().timestamp() - 1,
        return_to: Default::default(),
    };
    let callback_state = OidcCallbackState::parse(OidcCallbackState::encode(
        OidcFlowId::from_uuid(payload.flow_id),
        &payload.state,
    ))
    .unwrap();
    let encoded = seal_login_flow_cookie(&state, &master_key, &payload).unwrap();
    assert!(consume_login_flow_cookie(&state, &encoded, &callback_state).is_err());
    let mut tampered = encoded;
    tampered.push('x');
    assert!(consume_login_flow_cookie(&state, &tampered, &callback_state).is_err());

    let encoded = seal_login_flow_cookie(&state, &master_key, &payload).unwrap();
    for alias in ["v02.", "v+2."] {
        let aliased = encoded.replacen("v2.", alias, 1);
        assert!(consume_login_flow_cookie(&state, &aliased, &callback_state).is_err());
    }
}

#[tokio::test]
async fn callback_clears_a_login_cookie_when_query_extraction_fails() {
    let state = ManagementState::new(
        crate::ApiMode::Control,
        None,
        std::sync::Arc::new(crate::RuntimeManager::empty()),
        "https://console.example.test",
        std::path::PathBuf::from("missing-console"),
    );
    let response = crate::router::management_router_for_test(state)
        .oneshot(
            axum::http::Request::get("/api/v1/oidc/callback?code=one&code=two")
                .header(header::COOKIE, format!("{LOGIN_FLOW_COOKIE}=opaque"))
                .body(axum::body::Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::BAD_REQUEST);
    assert!(
        response
            .headers()
            .get_all(header::SET_COOKIE)
            .iter()
            .filter_map(|value| value.to_str().ok())
            .any(|value| value.starts_with(&format!("{LOGIN_FLOW_COOKIE}=;")))
    );
}

#[tokio::test]
async fn callback_clears_the_matching_scoped_cookie_when_query_extraction_fails() {
    let state = ManagementState::new(
        crate::ApiMode::Control,
        None,
        std::sync::Arc::new(crate::RuntimeManager::empty()),
        "https://console.example.test",
        std::path::PathBuf::from("missing-console"),
    );
    let flow_id = OidcFlowId::generate();
    let other_flow_id = OidcFlowId::generate();
    let state_value = OidcCallbackState::encode(flow_id, &"a".repeat(43));
    let flow_cookie = flow_cookie_name(OidcFlowPurpose::Login, flow_id);
    let other_flow_cookie = flow_cookie_name(OidcFlowPurpose::Login, other_flow_id);
    let response = crate::router::management_router_for_test(state)
        .oneshot(
            axum::http::Request::get(format!(
                "/api/v1/oidc/callback?state={state_value}&code=one&code=two"
            ))
            .header(
                header::COOKIE,
                format!("{flow_cookie}=opaque; {other_flow_cookie}=opaque"),
            )
            .body(axum::body::Body::empty())
            .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::BAD_REQUEST);
    let set_cookies = response
        .headers()
        .get_all(header::SET_COOKIE)
        .iter()
        .filter_map(|value| value.to_str().ok())
        .collect::<Vec<_>>();
    assert!(
        set_cookies
            .iter()
            .any(|value| value.starts_with(&format!("{flow_cookie}=;")))
    );
    assert!(
        !set_cookies
            .iter()
            .any(|value| value.starts_with(&format!("{other_flow_cookie}=;")))
    );
}

#[test]
fn asymmetric_validation_enforces_signature_issuer_audience_nonce_and_time() {
    let configuration = test_configuration();
    let now = Utc::now().timestamp();
    let valid_claims = json!({
        "iss": configuration.issuer,
        "sub": "subject",
        "aud": configuration.client_id,
        "iat": now,
        "exp": now + 300,
        "nonce": "expected-nonce",
        "email": "person@example.test",
        "email_verified": true,
        "groups": ["engineering"]
    });
    let jwks: JwkSet = serde_json::from_value(json!({"keys": [{
        "kty": "OKP", "crv": "Ed25519", "use": "sig", "alg": "EdDSA",
        "kid": "test-key", "x": ED25519_PUBLIC_X
    }]}))
    .unwrap();
    let valid_token = sign_ed_token(&valid_claims);
    assert!(
        validate_id_token(&valid_token, &jwks, &configuration, "expected-nonce", false).is_ok()
    );

    for (claim, invalid_value) in [
        ("iss", json!("https://other-issuer.example")),
        ("aud", json!("other-client")),
        ("iat", json!(now + 600)),
        ("iat", json!(now - 1_200)),
        ("exp", json!(now - 120)),
        ("nonce", json!("wrong-nonce")),
    ] {
        let mut claims = valid_claims.clone();
        claims[claim] = invalid_value;
        assert!(
            validate_id_token(
                &sign_ed_token(&claims),
                &jwks,
                &configuration,
                "expected-nonce",
                false,
            )
            .is_err(),
            "{claim} must be validated"
        );
    }

    let mut tampered_parts = valid_token
        .split('.')
        .map(str::to_owned)
        .collect::<Vec<_>>();
    let replacement = if tampered_parts[2].starts_with('A') {
        "B"
    } else {
        "A"
    };
    tampered_parts[2].replace_range(..1, replacement);
    assert!(
        validate_id_token(
            &tampered_parts.join("."),
            &jwks,
            &configuration,
            "expected-nonce",
            false,
        )
        .is_err()
    );
}

fn sign_ed_token(claims: &Value) -> String {
    let private_der = STANDARD.decode(ED25519_PRIVATE_DER_B64).unwrap();
    let mut header = Header::new(Algorithm::EdDSA);
    header.kid = Some("test-key".to_owned());
    encode(&header, claims, &EncodingKey::from_ed_der(&private_der)).unwrap()
}

fn test_configuration() -> OidcConfiguration {
    OidcConfiguration {
        id: Uuid::now_v7(),
        discovery_url: "https://idp.example/.well-known/openid-configuration".to_owned(),
        issuer: "https://idp.example".to_owned(),
        authorization_endpoint: "https://idp.example/authorize".to_owned(),
        token_endpoint: "https://idp.example/token".to_owned(),
        jwks_uri: "https://idp.example/jwks".to_owned(),
        token_endpoint_auth_method: "client_secret_basic".to_owned(),
        client_id: "olp".to_owned(),
        encrypted_client_secret: olp_storage::EncryptedSecret {
            key_version: 1,
            nonce: [0; 12],
            ciphertext: vec![0; 16],
        },
        scopes: default_scopes(),
        email_claim: default_email_claim(),
        groups_claim: default_groups_claim(),
        default_role: None,
        email_role_mappings: vec![],
        group_role_mappings: vec![],
        enabled: true,
        etag: Uuid::now_v7(),
        created_at: Utc::now(),
        updated_at: Utc::now(),
    }
}
