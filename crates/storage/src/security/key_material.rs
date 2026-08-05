use std::fmt;

use base64::{
    Engine as _,
    engine::general_purpose::{STANDARD, URL_SAFE, URL_SAFE_NO_PAD},
};
use hmac::{Hmac, Mac, digest::KeyInit};
use rand::RngCore;
use sha2::Sha256;
use zeroize::{Zeroize, Zeroizing};

use super::SecurityError;

type HmacSha256 = Hmac<Sha256>;

const API_KEY_PREFIX: &str = "olp_v2_";
const LOOKUP_BYTES: usize = 6;
const SECRET_BYTES: usize = 32;
const BOOTSTRAP_TOKEN_DOMAIN: &[u8] = b"olp:v2:bootstrap-setup-token:v1";

/// Authentication HMAC key used for proxy keys and public-auth identities.
/// This is intentionally distinct from the provider-credential encryption key
/// so the two can rotate separately.
pub struct AuthHmacKey([u8; 32]);

impl AuthHmacKey {
    #[must_use]
    pub fn new(bytes: [u8; 32]) -> Self {
        Self(bytes)
    }

    pub fn from_base64(encoded: &str) -> Result<Self, SecurityError> {
        Ok(Self(decode_key(encoded)?))
    }

    #[must_use]
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
        let mut mac = <HmacSha256 as KeyInit>::new_from_slice(&self.0)
            .expect("HMAC accepts keys of every size");
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

    /// Extracts only the public lookup component. Authentication must still
    /// call `parse_and_verify` with the digest loaded from the pinned snapshot.
    pub fn lookup_id<'a>(&self, plaintext: &'a str) -> Result<&'a str, SecurityError> {
        split_api_key(plaintext).map(|(lookup_id, _)| lookup_id)
    }

    /// Produces an opaque identity for a public-auth source. This is
    /// deliberately domain-separated from API-key material and from
    /// source-plus-target identities so rate-limit rows cannot be correlated
    /// or repurposed.
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
        let mut mac = <HmacSha256 as KeyInit>::new_from_slice(&self.0)
            .expect("HMAC accepts keys of every size");
        mac.update(domain);
        for part in parts {
            mac.update(&(part.len() as u64).to_be_bytes());
            mac.update(part);
        }
        mac
    }

    fn digest(&self, lookup_id: &str, secret: &[u8]) -> [u8; 32] {
        let mut mac = <HmacSha256 as KeyInit>::new_from_slice(&self.0)
            .expect("HMAC accepts keys of every size");
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
    #[must_use]
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

pub(super) fn decode_key(encoded: &str) -> Result<[u8; 32], SecurityError> {
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
