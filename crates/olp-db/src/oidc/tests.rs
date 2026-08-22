use chrono::{DateTime, Utc};
use olp_engine::domain::auth::Role;
use uuid::Uuid;

use super::helpers::{
    AuthenticatedUserRow, authenticated_user_from_row, checked_session_expiry, encrypted_from_row,
    normalize_display_name, normalize_email, required_string, role_rank, valid_claim_name,
    validate_subject,
};
use super::types::{
    OidcConfiguration, OidcError, OidcFlowMaterial, OidcFlowPurpose, OidcRoleMapping,
};
use crate::security::envelope::EncryptedSecret;

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

#[test]
fn flow_purpose_storage_names_round_trip_as_a_closed_set() {
    for (purpose, stored) in [
        (OidcFlowPurpose::Login, "login"),
        (OidcFlowPurpose::Link, "link"),
        (OidcFlowPurpose::Reauthenticate, "reauthenticate"),
    ] {
        assert_eq!(purpose.as_str(), stored);
        assert_eq!(OidcFlowPurpose::parse(stored).unwrap(), purpose);
    }
    assert!(matches!(
        OidcFlowPurpose::parse("unknown"),
        Err(OidcError::Corrupt)
    ));
}

#[test]
fn oidc_identity_text_normalization_is_bounded_and_deterministic() {
    assert_eq!(
        normalize_email(" Owner@Example.TEST ").unwrap(),
        "owner@example.test"
    );
    for invalid in [
        "",
        "missing-at",
        "@example.test",
        "owner@",
        "a\n@example.test",
    ] {
        assert!(normalize_email(invalid).is_err(), "accepted {invalid:?}");
    }

    assert_eq!(
        normalize_display_name(None, "person@example.test"),
        "person"
    );
    assert_eq!(
        normalize_display_name(Some("  Person Name  "), "ignored@example.test"),
        "Person Name"
    );
    assert_eq!(
        normalize_display_name(Some(&"é".repeat(101)), "ignored@example.test")
            .chars()
            .count(),
        100
    );

    assert!(validate_subject("subject-123").is_ok());
    for invalid in ["", "subject\ncontrol"] {
        assert!(validate_subject(invalid).is_err());
    }
    assert!(validate_subject(&"s".repeat(256)).is_err());

    for valid in ["email", "realm.groups", "custom:groups", "claim-name_2"] {
        assert!(valid_claim_name(valid), "rejected {valid:?}");
    }
    for invalid in ["", "claim/name", "claim name"] {
        assert!(!valid_claim_name(invalid), "accepted {invalid:?}");
    }
    assert!(!valid_claim_name(&"c".repeat(129)));
}

#[test]
fn oidc_row_helpers_fail_closed_on_corrupt_storage() {
    let encrypted = encrypted_from_row(2, vec![7; 12], vec![8; 16]).unwrap();
    assert_eq!(encrypted.key_version, 2);
    assert_eq!(encrypted.nonce, [7; 12]);
    assert_eq!(encrypted.ciphertext, vec![8; 16]);
    assert!(encrypted_from_row(-1, vec![0; 12], vec![]).is_err());
    assert!(encrypted_from_row(1, vec![0; 11], vec![]).is_err());

    assert_eq!(required_string(Some("value".to_owned())).unwrap(), "value");
    assert!(required_string(None).is_err());
    assert!(required_string(Some(String::new())).is_err());

    let row = AuthenticatedUserRow {
        id: Uuid::now_v7(),
        email: "owner@example.test".to_owned(),
        display_name: "Owner".to_owned(),
        role: "owner".to_owned(),
    };
    assert_eq!(authenticated_user_from_row(row).unwrap().role, Role::Owner);
    let corrupt = AuthenticatedUserRow {
        id: Uuid::now_v7(),
        email: "owner@example.test".to_owned(),
        display_name: "Owner".to_owned(),
        role: "superuser".to_owned(),
    };
    assert!(matches!(
        authenticated_user_from_row(corrupt),
        Err(OidcError::Corrupt)
    ));
}

#[test]
fn session_expiry_and_role_ranking_cover_security_boundaries() {
    let now = Utc::now();
    assert_eq!(
        checked_session_expiry(now, chrono::Duration::minutes(5)).unwrap(),
        now + chrono::Duration::minutes(5)
    );
    assert!(checked_session_expiry(now, chrono::Duration::zero()).is_err());
    assert!(checked_session_expiry(now, chrono::Duration::seconds(-1)).is_err());
    assert!(
        checked_session_expiry(DateTime::<Utc>::MAX_UTC, chrono::Duration::seconds(1)).is_err()
    );

    assert_eq!(
        [Role::Owner, Role::Operator, Role::Developer, Role::Viewer].map(role_rank),
        [0, 1, 2, 3]
    );
}
