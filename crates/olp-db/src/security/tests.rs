use base64::{Engine as _, engine::general_purpose::STANDARD};
use uuid::Uuid;

use super::{
    AuthHmacKey, CsrfMaterial, InvitationMaterial, MasterKey, RecentAuthMaterial, SecurityError,
    SessionMaterial, constant_time_eq, credential_aad, hash_password, idempotency_replay_aad,
    idempotency_replay_scope, oidc_client_secret_aad, oidc_flow_payload_aad, verify_password,
};

#[test]
fn durable_encryption_scopes_have_stable_domain_separated_encodings() {
    let first = Uuid::parse_str("018f47f2-d154-7c52-8170-7a16f378bb29").unwrap();
    let second = Uuid::parse_str("018f47f2-d154-7c52-8170-7a16f378bb2a").unwrap();

    assert_eq!(
        credential_aad(first, second, 3).as_slice(),
        b"olp:v2:provider:018f47f2-d154-7c52-8170-7a16f378bb29:credential:018f47f2-d154-7c52-8170-7a16f378bb2a:v3"
    );
    assert_eq!(
        oidc_client_secret_aad(first).as_slice(),
        b"olp:v2:oidc:018f47f2-d154-7c52-8170-7a16f378bb29:client-secret"
    );
    assert_eq!(
        oidc_flow_payload_aad(second).as_slice(),
        b"olp:v2:oidc-flow:018f47f2-d154-7c52-8170-7a16f378bb2a"
    );

    let scope = idempotency_replay_scope(first, "create-route", "request-key");
    assert_eq!(
        scope,
        format!("olp:v2:idempotency:{first}:create-route:request-key")
    );
    assert_eq!(
        idempotency_replay_aad(first, "create-route", "request-key"),
        scope.into_bytes()
    );
}

#[test]
fn constant_time_comparison_handles_content_and_length_mismatches() {
    for (left, right, equal) in [
        (b"".as_slice(), b"".as_slice(), true),
        (b"digest".as_slice(), b"digest".as_slice(), true),
        (b"digest".as_slice(), b"digesu".as_slice(), false),
        (b"digest".as_slice(), b"digest-long".as_slice(), false),
    ] {
        assert_eq!(constant_time_eq(left, right), equal);
    }
}

#[test]
fn credential_encryption_binds_ciphertext_to_context() {
    let key = MasterKey::new(7, [42; 32]);
    let encrypted = key.seal(b"provider-secret", b"provider:123:v1").unwrap();

    assert_eq!(
        key.open(&encrypted, b"provider:123:v1").unwrap().as_slice(),
        b"provider-secret"
    );
    assert!(key.open(&encrypted, b"provider:999:v1").is_err());
    assert!(!format!("{key:?}").contains("42"));
}

#[test]
fn versioned_keyring_rotates_writes_without_losing_old_envelopes() {
    let version_one = STANDARD.encode([1_u8; 32]);
    let version_two = STANDARD.encode([2_u8; 32]);
    let first = MasterKey::from_file_contents(&format!(
        r#"{{"active_version":1,"keys":[{{"version":1,"key":"{version_one}"}}]}}"#
    ))
    .unwrap();
    let old = first.seal(b"old-secret", b"provider:v1").unwrap();
    assert_eq!(old.key_version, 1);

    let rotated = MasterKey::from_file_contents(&format!(
        r#"{{"active_version":2,"keys":[{{"version":1,"key":"{version_one}"}},{{"version":2,"key":"{version_two}"}}]}}"#
    ))
    .unwrap();
    assert_eq!(
        rotated.open(&old, b"provider:v1").unwrap().as_slice(),
        b"old-secret"
    );
    let new = rotated.seal(b"new-secret", b"provider:v2").unwrap();
    assert_eq!(new.key_version, 2);
    assert_eq!(
        rotated.open(&new, b"provider:v2").unwrap().as_slice(),
        b"new-secret"
    );
    let resealed = rotated.reseal(&old, b"provider:v1").unwrap();
    assert_eq!(resealed.key_version, 2);
    assert_eq!(
        rotated.open(&resealed, b"provider:v1").unwrap().as_slice(),
        b"old-secret"
    );
    assert_eq!(rotated.versions().collect::<Vec<_>>(), vec![1, 2]);

    let mut tampered = old.clone();
    tampered.ciphertext[0] ^= 1;
    assert!(rotated.reseal(&tampered, b"provider:v1").is_err());

    let version_two_only = MasterKey::from_file_contents(&format!(
        r#"{{"active_version":2,"keys":[{{"version":2,"key":"{version_two}"}}]}}"#
    ))
    .unwrap();
    assert!(version_two_only.open(&old, b"provider:v1").is_err());
    assert!(version_two_only.open(&resealed, b"provider:v1").is_ok());
    assert!(version_two_only.open(&new, b"provider:v2").is_ok());
}

#[test]
fn master_key_file_is_strict_and_legacy_base64_remains_supported() {
    let encoded = STANDARD.encode([7_u8; 32]);
    assert_eq!(
        MasterKey::from_file_contents(&encoded).unwrap().version(),
        1
    );
    assert!(matches!(
        MasterKey::from_file_contents(&format!(
            r#"{{"active_version":2,"keys":[{{"version":1,"key":"{encoded}"}}]}}"#
        )),
        Err(SecurityError::MissingActiveMasterKey)
    ));
    assert!(matches!(
        MasterKey::from_file_contents(&format!(
            r#"{{"active_version":1,"keys":[{{"version":1,"key":"{encoded}"}},{{"version":1,"key":"{encoded}"}}]}}"#
        )),
        Err(SecurityError::InvalidMasterKeyVersion)
    ));
    assert!(
        MasterKey::from_file_contents(r#"{"active_version":1,"keys":[],"unexpected":true}"#)
            .is_err()
    );
}

#[test]
fn proxy_keys_are_lookupable_and_hmac_verified() {
    let auth_hmac_key = AuthHmacKey::new([9; 32]);
    let generated = auth_hmac_key.generate_api_key();
    let parsed = auth_hmac_key
        .parse_and_verify(generated.expose_once(), &generated.digest)
        .unwrap();

    assert_eq!(parsed.lookup_id, generated.lookup_id);
    assert_eq!(generated.expose_once().len(), 7 + 12 + 1 + 43);

    let mut tampered = generated.expose_once().to_owned();
    tampered.push('a');
    assert!(
        auth_hmac_key
            .parse_and_verify(&tampered, &generated.digest)
            .is_err()
    );
    assert!(!format!("{generated:?}").contains(generated.expose_once()));
}

#[test]
fn public_auth_and_bootstrap_digests_are_domain_separated() {
    let auth_hmac_key = AuthHmacKey::new([9; 32]);
    let source = auth_hmac_key.public_auth_source_digest("203.0.113.10");
    let source_target =
        auth_hmac_key.public_auth_source_target_digest("203.0.113.10", "owner@example.test");
    assert_ne!(source, source_target);
    assert_ne!(
        source_target,
        auth_hmac_key.public_auth_source_target_digest("203.0.113.11", "owner@example.test")
    );

    let token = STANDARD.encode([7_u8; 32]);
    let digest = auth_hmac_key
        .bootstrap_token_digest_from_base64(&token)
        .unwrap();
    assert!(auth_hmac_key.verify_bootstrap_token_digest(&token, &digest));
    assert!(!auth_hmac_key.verify_bootstrap_token_digest(&STANDARD.encode([8_u8; 32]), &digest));
    assert!(
        auth_hmac_key
            .bootstrap_token_digest_from_base64(&STANDARD.encode([1_u8; 31]))
            .is_err()
    );
}

#[test]
fn passwords_use_argon2id_and_verify() {
    let encoded = hash_password("correct horse battery staple").unwrap();
    assert!(encoded.starts_with("$argon2id$"));
    assert!(verify_password("correct horse battery staple", &encoded));
    assert!(!verify_password("incorrect", &encoded));
}

#[test]
fn session_tokens_have_independent_csrf_material() {
    let material = SessionMaterial::generate();
    assert_ne!(material.token(), material.csrf_token());
    assert_eq!(
        material.token_digest(),
        SessionMaterial::digest_token(material.token())
    );
    assert!(SessionMaterial::verify_csrf(
        material.csrf_token(),
        &material.csrf_digest()
    ));
    assert!(!SessionMaterial::verify_csrf(
        material.token(),
        &material.csrf_digest()
    ));
    assert!(!format!("{material:?}").contains(material.token()));
}

#[test]
fn one_time_tokens_are_random_digest_only_material() {
    let first = InvitationMaterial::generate();
    let second = InvitationMaterial::generate();
    assert_ne!(first.token(), second.token());
    assert_eq!(
        first.token_digest(),
        InvitationMaterial::digest_token(first.token())
    );

    let recent = RecentAuthMaterial::generate();
    assert_eq!(
        recent.token_digest(),
        RecentAuthMaterial::digest_token(recent.token())
    );
    let csrf = CsrfMaterial::generate();

    for (token, digest, debug) in [
        (first.token(), first.token_digest(), format!("{first:?}")),
        (recent.token(), recent.token_digest(), format!("{recent:?}")),
        (csrf.token(), csrf.token_digest(), format!("{csrf:?}")),
    ] {
        assert_eq!(token.len(), 43);
        assert_eq!(digest.len(), 32);
        assert!(!debug.contains(token));
    }
}
