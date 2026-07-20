use super::*;
use chrono::Utc;
use olp_domain::Role;
use uuid::Uuid;

use crate::EncryptedSecret;

fn mapping(value: &str, role: Role) -> OidcRoleMapping {
    OidcRoleMapping {
        claim_value: value.to_owned(),
        role,
    }
}

#[test]
fn flow_material_has_s256_challenge_and_redacted_debug() {
    let material = OidcFlowMaterial::generate();
    assert_eq!(material.state().len(), 43);
    assert_eq!(material.browser_binding().len(), 43);
    assert_eq!(material.nonce().len(), 43);
    assert_eq!(material.pkce_verifier().len(), 43);
    assert_eq!(material.pkce_challenge().len(), 43);
    assert!(!format!("{material:?}").contains(material.state()));
}

#[test]
fn mapping_precedence_is_exact_email_then_strongest_group_then_default() {
    let configuration = OidcConfiguration {
        id: Uuid::now_v7(),
        discovery_url: "https://idp.example/.well-known/openid-configuration".to_owned(),
        issuer: "https://idp.example".to_owned(),
        authorization_endpoint: "https://idp.example/authorize".to_owned(),
        token_endpoint: "https://idp.example/token".to_owned(),
        jwks_uri: "https://idp.example/jwks".to_owned(),
        token_endpoint_auth_method: "client_secret_basic".to_owned(),
        client_id: "olp".to_owned(),
        encrypted_client_secret: EncryptedSecret {
            key_version: 1,
            nonce: [0; 12],
            ciphertext: vec![0; 16],
        },
        scopes: vec!["openid".to_owned()],
        email_claim: "email".to_owned(),
        groups_claim: "groups".to_owned(),
        default_role: Some(Role::Viewer),
        email_role_mappings: vec![mapping("owner@example.test", Role::Owner)],
        group_role_mappings: vec![
            mapping("engineering", Role::Developer),
            mapping("operations", Role::Operator),
        ],
        enabled: true,
        etag: Uuid::now_v7(),
        created_at: Utc::now(),
        updated_at: Utc::now(),
    };
    assert_eq!(
        configuration.mapped_role("OWNER@example.test", &["engineering".to_owned()]),
        Some(Role::Owner)
    );
    assert_eq!(
        configuration.mapped_role(
            "person@example.test",
            &["engineering".to_owned(), "operations".to_owned()]
        ),
        Some(Role::Operator)
    );
    assert_eq!(
        configuration.mapped_role("person@example.test", &[]),
        Some(Role::Viewer)
    );
}
