use std::{collections::BTreeMap, fmt};

use aes_gcm::{
    Aes256Gcm, Nonce,
    aead::{Aead, KeyInit, Payload},
};
use argon2::{
    Algorithm, Argon2, Params, Version,
    password_hash::{PasswordHash, PasswordHasher, PasswordVerifier, SaltString, rand_core::OsRng},
};
use base64::{
    Engine as _,
    engine::general_purpose::{STANDARD, URL_SAFE, URL_SAFE_NO_PAD},
};
use hmac::{Hmac, Mac};
use rand::RngCore;
use serde::Deserialize;
use sha2::{Digest, Sha256};
use thiserror::Error;
use uuid::Uuid;
use zeroize::{Zeroize, Zeroizing};

type HmacSha256 = Hmac<Sha256>;

const API_KEY_PREFIX: &str = "olp_v2_";
const LOOKUP_BYTES: usize = 6;
const SECRET_BYTES: usize = 32;
const NONCE_BYTES: usize = 12;
const BOOTSTRAP_TOKEN_DOMAIN: &[u8] = b"olp:v2:bootstrap-setup-token:v1";

#[must_use]
pub fn credential_aad(provider_id: Uuid, credential_id: Uuid, version: u32) -> Vec<u8> {
    format!("olp:v2:provider:{provider_id}:credential:{credential_id}:v{version}").into_bytes()
}

#[must_use]
pub fn oidc_client_secret_aad(configuration_id: Uuid) -> Vec<u8> {
    format!("olp:v2:oidc:{configuration_id}:client-secret").into_bytes()
}

#[must_use]
pub fn oidc_flow_payload_aad(flow_id: Uuid) -> Vec<u8> {
    format!("olp:v2:oidc-flow:{flow_id}").into_bytes()
}

#[must_use]
pub fn idempotency_replay_aad(actor: Uuid, operation: &str, key: &str) -> Vec<u8> {
    idempotency_replay_scope(actor, operation, key).into_bytes()
}

#[must_use]
pub fn idempotency_replay_scope(actor: Uuid, operation: &str, key: &str) -> String {
    format!("olp:v2:idempotency:{actor}:{operation}:{key}")
}

#[derive(Debug, Error)]
pub enum SecurityError {
    #[error("master key must be exactly 32 bytes")]
    InvalidMasterKey,
    #[error("master-key file is invalid")]
    InvalidMasterKeyFile,
    #[error("master-key versions must be unique positive integers")]
    InvalidMasterKeyVersion,
    #[error("active master-key version is not present in the keyring")]
    MissingActiveMasterKey,
    #[error("secret has an invalid format")]
    InvalidSecretFormat,
    #[error("secret encryption failed")]
    Encryption,
    #[error("secret decryption failed")]
    Decryption,
    #[error("password hashing failed")]
    PasswordHash,
}

/// A rotatable AES-256-GCM master key loaded from a mounted secret file.
pub struct MasterKey {
    active_version: u32,
    keys: BTreeMap<u32, [u8; 32]>,
}

impl MasterKey {
    pub fn new(version: u32, bytes: [u8; 32]) -> Self {
        Self {
            active_version: version,
            keys: BTreeMap::from([(version, bytes)]),
        }
    }

    /// Loads either the legacy single-key base64 format (version 1) or a
    /// versioned JSON keyring. The active key encrypts new values; retained
    /// versions are decrypt-only and allow zero-downtime rotation.
    pub fn from_file_contents(contents: &str) -> Result<Self, SecurityError> {
        let trimmed = contents.trim();
        if !trimmed.starts_with('{') {
            return Ok(Self::new(1, decode_key(trimmed)?));
        }
        let mut document: MasterKeyFile =
            serde_json::from_str(trimmed).map_err(|_| SecurityError::InvalidMasterKeyFile)?;
        if document.active_version == 0 || document.keys.is_empty() || document.keys.len() > 32 {
            document.zeroize();
            return Err(SecurityError::InvalidMasterKeyFile);
        }
        let mut keys = BTreeMap::new();
        for entry in &mut document.keys {
            if entry.version == 0 || keys.contains_key(&entry.version) {
                document.zeroize();
                zeroize_key_map(&mut keys);
                return Err(SecurityError::InvalidMasterKeyVersion);
            }
            let decoded = match decode_key(&entry.key) {
                Ok(decoded) => decoded,
                Err(error) => {
                    zeroize_key_map(&mut keys);
                    return Err(error);
                }
            };
            entry.key.zeroize();
            keys.insert(entry.version, decoded);
        }
        if !keys.contains_key(&document.active_version) {
            document.zeroize();
            zeroize_key_map(&mut keys);
            return Err(SecurityError::MissingActiveMasterKey);
        }
        let active_version = document.active_version;
        document.zeroize();
        Ok(Self {
            active_version,
            keys,
        })
    }

    pub fn version(&self) -> u32 {
        self.active_version
    }

    pub fn versions(&self) -> impl Iterator<Item = u32> + '_ {
        self.keys.keys().copied()
    }

    #[must_use]
    pub fn contains_version(&self, version: u32) -> bool {
        self.keys.contains_key(&version)
    }

    pub fn seal(&self, plaintext: &[u8], aad: &[u8]) -> Result<EncryptedSecret, SecurityError> {
        let bytes = self
            .keys
            .get(&self.active_version)
            .ok_or(SecurityError::MissingActiveMasterKey)?;
        let cipher =
            Aes256Gcm::new_from_slice(bytes).map_err(|_| SecurityError::InvalidMasterKey)?;
        let mut nonce = [0_u8; NONCE_BYTES];
        rand::rng().fill_bytes(&mut nonce);
        let ciphertext = cipher
            .encrypt(
                Nonce::from_slice(&nonce),
                Payload {
                    msg: plaintext,
                    aad,
                },
            )
            .map_err(|_| SecurityError::Encryption)?;

        Ok(EncryptedSecret {
            key_version: self.active_version,
            nonce,
            ciphertext,
        })
    }

    pub fn open(
        &self,
        encrypted: &EncryptedSecret,
        aad: &[u8],
    ) -> Result<Zeroizing<Vec<u8>>, SecurityError> {
        let bytes = self
            .keys
            .get(&encrypted.key_version)
            .ok_or(SecurityError::Decryption)?;
        let cipher =
            Aes256Gcm::new_from_slice(bytes).map_err(|_| SecurityError::InvalidMasterKey)?;
        let plaintext = cipher
            .decrypt(
                Nonce::from_slice(&encrypted.nonce),
                Payload {
                    msg: &encrypted.ciphertext,
                    aad,
                },
            )
            .map_err(|_| SecurityError::Decryption)?;
        Ok(Zeroizing::new(plaintext))
    }

    /// Authenticates and decrypts with the envelope's referenced version, then
    /// immediately re-encrypts with the active version. Plaintext remains in
    /// zeroizing memory and is never formatted or returned to callers.
    pub fn reseal(
        &self,
        encrypted: &EncryptedSecret,
        aad: &[u8],
    ) -> Result<EncryptedSecret, SecurityError> {
        let plaintext = self.open(encrypted, aad)?;
        self.seal(&plaintext, aad)
    }
}

fn zeroize_key_map(keys: &mut BTreeMap<u32, [u8; 32]>) {
    for key in keys.values_mut() {
        key.zeroize();
    }
}

impl fmt::Debug for MasterKey {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("MasterKey")
            .field("active_version", &self.active_version)
            .field("key_versions", &self.keys.keys().collect::<Vec<_>>())
            .field("keys", &"[REDACTED]")
            .finish()
    }
}

impl Drop for MasterKey {
    fn drop(&mut self) {
        for key in self.keys.values_mut() {
            key.zeroize();
        }
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct MasterKeyFile {
    active_version: u32,
    keys: Vec<MasterKeyFileEntry>,
}

impl MasterKeyFile {
    fn zeroize(&mut self) {
        for entry in &mut self.keys {
            entry.key.zeroize();
        }
    }
}

impl Drop for MasterKeyFile {
    fn drop(&mut self) {
        self.zeroize();
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct MasterKeyFileEntry {
    version: u32,
    key: String,
}

#[derive(Clone, PartialEq, Eq)]
pub struct EncryptedSecret {
    pub key_version: u32,
    pub nonce: [u8; NONCE_BYTES],
    pub ciphertext: Vec<u8>,
}

impl fmt::Debug for EncryptedSecret {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("EncryptedSecret")
            .field("key_version", &self.key_version)
            .field("nonce", &"[REDACTED]")
            .field("ciphertext", &"[REDACTED]")
            .finish()
    }
}

/// Authentication HMAC key used for proxy keys and public-auth identities. This
/// is intentionally distinct from the provider-credential encryption key so the
/// two can rotate separately.
pub struct AuthHmacKey([u8; 32]);

impl AuthHmacKey {
    pub fn new(bytes: [u8; 32]) -> Self {
        Self(bytes)
    }

    pub fn from_base64(encoded: &str) -> Result<Self, SecurityError> {
        Ok(Self(decode_key(encoded)?))
    }

    pub fn generate_api_key(&self) -> ApiKeyMaterial {
        let mut lookup = [0_u8; LOOKUP_BYTES];
        let mut secret = [0_u8; SECRET_BYTES];
        rand::rng().fill_bytes(&mut lookup);
        rand::rng().fill_bytes(&mut secret);

        let lookup_id = hex_lower(&lookup);
        let secret_encoded = URL_SAFE_NO_PAD.encode(secret);
        let plaintext = Zeroizing::new(format!("{API_KEY_PREFIX}{lookup_id}_{secret_encoded}"));
        let digest = self.digest(&lookup_id, &secret);
        secret.zeroize();

        ApiKeyMaterial {
            lookup_id,
            digest,
            plaintext,
        }
    }

    pub fn parse_and_verify(
        &self,
        plaintext: &str,
        expected_digest: &[u8],
    ) -> Result<ParsedApiKey, SecurityError> {
        let (lookup_id, encoded_secret) = split_api_key(plaintext)?;
        let mut secret = Zeroizing::new(
            URL_SAFE_NO_PAD
                .decode(encoded_secret)
                .map_err(|_| SecurityError::InvalidSecretFormat)?,
        );
        if secret.len() != SECRET_BYTES {
            return Err(SecurityError::InvalidSecretFormat);
        }
        let mut mac =
            <HmacSha256 as Mac>::new_from_slice(&self.0).expect("HMAC accepts keys of every size");
        mac.update(API_KEY_PREFIX.as_bytes());
        mac.update(lookup_id.as_bytes());
        mac.update(&secret);
        mac.verify_slice(expected_digest)
            .map_err(|_| SecurityError::InvalidSecretFormat)?;
        secret.zeroize();
        Ok(ParsedApiKey {
            lookup_id: lookup_id.to_owned(),
        })
    }

    /// Extracts only the public lookup component. Authentication must still call
    /// `parse_and_verify` with the digest loaded from the pinned snapshot.
    pub fn lookup_id<'a>(&self, plaintext: &'a str) -> Result<&'a str, SecurityError> {
        split_api_key(plaintext).map(|(lookup_id, _)| lookup_id)
    }

    /// Produces an opaque identity for a public-auth source. This is deliberately
    /// domain-separated from API-key material and from source-plus-target
    /// identities so rate-limit rows cannot be correlated or repurposed.
    #[must_use]
    pub fn public_auth_source_digest(&self, source: &str) -> [u8; 32] {
        self.scoped_digest(b"olp:v2:public-auth:source:v1", &[source.as_bytes()])
    }

    /// Produces an opaque identity for a public-auth source attempting a
    /// particular target (an email address or invitation token). Both values
    /// are length-framed before authentication to avoid ambiguous joins.
    #[must_use]
    pub fn public_auth_source_target_digest(&self, source: &str, target: &str) -> [u8; 32] {
        self.scoped_digest(
            b"olp:v2:public-auth:source-target:v1",
            &[source.as_bytes(), target.as_bytes()],
        )
    }

    /// Returns a non-reversible bootstrap-token digest. The token file and
    /// request header use standard base64 for a precisely 32-byte token.
    pub fn bootstrap_token_digest_from_base64(
        &self,
        encoded: &str,
    ) -> Result<[u8; 32], SecurityError> {
        let token = Self::decode_bootstrap_token(encoded)?;
        Ok(self.scoped_digest(BOOTSTRAP_TOKEN_DOMAIN, &[&token]))
    }

    /// Checks a base64 bootstrap token with the HMAC implementation's
    /// constant-time verifier. Callers retain only the expected digest.
    #[must_use]
    pub fn verify_bootstrap_token_digest(&self, encoded: &str, expected: &[u8; 32]) -> bool {
        let Ok(token) = Self::decode_bootstrap_token(encoded) else {
            return false;
        };
        self.scoped_mac(BOOTSTRAP_TOKEN_DOMAIN, &[&token])
            .verify_slice(expected)
            .is_ok()
    }

    fn decode_bootstrap_token(encoded: &str) -> Result<Zeroizing<Vec<u8>>, SecurityError> {
        let token = Zeroizing::new(
            STANDARD
                .decode(encoded.trim())
                .map_err(|_| SecurityError::InvalidSecretFormat)?,
        );
        if token.len() != SECRET_BYTES {
            return Err(SecurityError::InvalidSecretFormat);
        }
        Ok(token)
    }

    fn scoped_digest(&self, domain: &[u8], parts: &[&[u8]]) -> [u8; 32] {
        self.scoped_mac(domain, parts)
            .finalize()
            .into_bytes()
            .into()
    }

    fn scoped_mac(&self, domain: &[u8], parts: &[&[u8]]) -> HmacSha256 {
        let mut mac =
            <HmacSha256 as Mac>::new_from_slice(&self.0).expect("HMAC accepts keys of every size");
        mac.update(domain);
        for part in parts {
            mac.update(&(part.len() as u64).to_be_bytes());
            mac.update(part);
        }
        mac
    }

    fn digest(&self, lookup_id: &str, secret: &[u8]) -> [u8; 32] {
        let mut mac =
            <HmacSha256 as Mac>::new_from_slice(&self.0).expect("HMAC accepts keys of every size");
        mac.update(API_KEY_PREFIX.as_bytes());
        mac.update(lookup_id.as_bytes());
        mac.update(secret);
        mac.finalize().into_bytes().into()
    }
}

impl fmt::Debug for AuthHmacKey {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("AuthHmacKey([REDACTED])")
    }
}

impl Drop for AuthHmacKey {
    fn drop(&mut self) {
        self.0.zeroize();
    }
}

pub struct ApiKeyMaterial {
    pub lookup_id: String,
    pub digest: [u8; 32],
    plaintext: Zeroizing<String>,
}

impl ApiKeyMaterial {
    /// The plaintext is returned only to the key-creation response. It is never
    /// serialized by a repository or included in Debug output.
    pub fn expose_once(&self) -> &str {
        &self.plaintext
    }
}

impl fmt::Debug for ApiKeyMaterial {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ApiKeyMaterial")
            .field("lookup_id", &self.lookup_id)
            .field("digest", &"[REDACTED]")
            .field("plaintext", &"[REDACTED]")
            .finish()
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ParsedApiKey {
    pub lookup_id: String,
}

pub struct SessionMaterial {
    token: Zeroizing<String>,
    csrf_token: Zeroizing<String>,
}

/// One-time, purpose-bound proof of recent authentication. Only its SHA-256
/// digest is stored on the exact session that requested the proof.
pub struct RecentAuthMaterial {
    token: Zeroizing<String>,
}

/// Replacement CSRF bearer used when an otherwise valid session has lost or
/// corrupted its readable CSRF cookie.
pub struct CsrfMaterial {
    token: Zeroizing<String>,
}

/// One-time invitation bearer material. Only its SHA-256 digest is persisted;
/// the plaintext is returned by the create-invitation API exactly once.
pub struct InvitationMaterial {
    token: Zeroizing<String>,
}

impl InvitationMaterial {
    #[must_use]
    pub fn generate() -> Self {
        Self {
            token: Zeroizing::new(random_token(32)),
        }
    }

    #[must_use]
    pub fn token(&self) -> &str {
        &self.token
    }

    #[must_use]
    pub fn token_digest(&self) -> [u8; 32] {
        Self::digest_token(&self.token)
    }

    #[must_use]
    pub fn digest_token(token: &str) -> [u8; 32] {
        Sha256::digest(token.as_bytes()).into()
    }
}

impl fmt::Debug for InvitationMaterial {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("InvitationMaterial([REDACTED])")
    }
}

impl RecentAuthMaterial {
    #[must_use]
    pub fn generate() -> Self {
        Self {
            token: Zeroizing::new(random_token(32)),
        }
    }

    #[must_use]
    pub fn token(&self) -> &str {
        &self.token
    }

    #[must_use]
    pub fn token_digest(&self) -> [u8; 32] {
        Self::digest_token(&self.token)
    }

    #[must_use]
    pub fn digest_token(token: &str) -> [u8; 32] {
        Sha256::digest(token.as_bytes()).into()
    }
}

impl fmt::Debug for RecentAuthMaterial {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("RecentAuthMaterial([REDACTED])")
    }
}

impl CsrfMaterial {
    #[must_use]
    pub fn generate() -> Self {
        Self {
            token: Zeroizing::new(random_token(32)),
        }
    }

    #[must_use]
    pub fn token(&self) -> &str {
        &self.token
    }

    #[must_use]
    pub fn token_digest(&self) -> [u8; 32] {
        Sha256::digest(self.token.as_bytes()).into()
    }
}

impl fmt::Debug for CsrfMaterial {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("CsrfMaterial([REDACTED])")
    }
}

impl SessionMaterial {
    pub fn generate() -> Self {
        Self {
            token: Zeroizing::new(random_token(32)),
            csrf_token: Zeroizing::new(random_token(32)),
        }
    }

    pub fn token(&self) -> &str {
        &self.token
    }

    pub fn csrf_token(&self) -> &str {
        &self.csrf_token
    }

    pub fn token_digest(&self) -> [u8; 32] {
        Sha256::digest(self.token.as_bytes()).into()
    }

    pub fn csrf_digest(&self) -> [u8; 32] {
        Sha256::digest(self.csrf_token.as_bytes()).into()
    }

    pub fn digest_token(token: &str) -> [u8; 32] {
        Sha256::digest(token.as_bytes()).into()
    }

    pub fn verify_csrf(token: &str, expected_digest: &[u8]) -> bool {
        let actual: [u8; 32] = Sha256::digest(token.as_bytes()).into();
        constant_time_eq(&actual, expected_digest)
    }
}

impl fmt::Debug for SessionMaterial {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("SessionMaterial([REDACTED])")
    }
}

pub fn hash_password(password: &str) -> Result<String, SecurityError> {
    let params = Params::new(19_456, 2, 1, Some(32)).map_err(|_| SecurityError::PasswordHash)?;
    let argon2 = Argon2::new(Algorithm::Argon2id, Version::V0x13, params);
    let salt = SaltString::generate(&mut OsRng);
    argon2
        .hash_password(password.as_bytes(), &salt)
        .map(|hash| hash.to_string())
        .map_err(|_| SecurityError::PasswordHash)
}

pub fn verify_password(password: &str, encoded: &str) -> bool {
    let Ok(hash) = PasswordHash::new(encoded) else {
        return false;
    };
    Argon2::default()
        .verify_password(password.as_bytes(), &hash)
        .is_ok()
}

fn random_token(size: usize) -> String {
    let mut bytes = Zeroizing::new(vec![0_u8; size]);
    rand::rng().fill_bytes(&mut bytes);
    URL_SAFE_NO_PAD.encode(&bytes)
}

fn decode_key(encoded: &str) -> Result<[u8; 32], SecurityError> {
    let trimmed = encoded.trim();
    let decoded = URL_SAFE_NO_PAD
        .decode(trimmed)
        .or_else(|_| URL_SAFE.decode(trimmed))
        .or_else(|_| STANDARD.decode(trimmed))
        .map_err(|_| SecurityError::InvalidMasterKey)?;
    let decoded = Zeroizing::new(decoded);
    decoded
        .as_slice()
        .try_into()
        .map_err(|_| SecurityError::InvalidMasterKey)
}

fn split_api_key(plaintext: &str) -> Result<(&str, &str), SecurityError> {
    let value = plaintext
        .strip_prefix(API_KEY_PREFIX)
        .ok_or(SecurityError::InvalidSecretFormat)?;
    let (lookup_id, encoded_secret) = value
        .split_once('_')
        .ok_or(SecurityError::InvalidSecretFormat)?;
    if lookup_id.len() != LOOKUP_BYTES * 2
        || !lookup_id.bytes().all(|byte| byte.is_ascii_hexdigit())
    {
        return Err(SecurityError::InvalidSecretFormat);
    }
    Ok((lookup_id, encoded_secret))
}

fn hex_lower(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut output = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        output.push(HEX[(byte >> 4) as usize] as char);
        output.push(HEX[(byte & 0x0f) as usize] as char);
    }
    output
}

pub fn constant_time_eq(left: &[u8], right: &[u8]) -> bool {
    if left.len() != right.len() {
        return false;
    }
    left.iter()
        .zip(right)
        .fold(0_u8, |difference, (a, b)| difference | (a ^ b))
        == 0
}

#[cfg(test)]
mod tests {
    use super::*;

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
        assert!(
            !auth_hmac_key.verify_bootstrap_token_digest(&STANDARD.encode([8_u8; 32]), &digest)
        );
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
}
