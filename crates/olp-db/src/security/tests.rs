use base64::{Engine as _, engine::general_purpose::STANDARD};

use super::{
    AuthHmacKey, InvitationMaterial, MasterKey, SecurityError, SessionMaterial, hash_password,
    verify_password,
};

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
    assert!(SessionMaterial::verify_csrf(
        material.csrf_token(),
        &material.csrf_digest()
    ));
    assert!(!SessionMaterial::verify_csrf(
        material.token(),
        &material.csrf_digest()
    ));
}

#[test]
fn invitation_tokens_are_random_digest_only_material() {
    let first = InvitationMaterial::generate();
    let second = InvitationMaterial::generate();

    assert_ne!(first.token(), second.token());
    assert_eq!(
        first.token_digest(),
        InvitationMaterial::digest_token(first.token())
    );
    assert_eq!(first.token_digest().len(), 32);
    assert!(!format!("{first:?}").contains(first.token()));
}
