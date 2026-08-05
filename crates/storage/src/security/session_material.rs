use std::fmt;

use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use rand::RngCore;
use sha2::{Digest, Sha256};
use zeroize::Zeroizing;

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
    #[must_use]
    pub fn generate() -> Self {
        Self {
            token: Zeroizing::new(random_token(32)),
            csrf_token: Zeroizing::new(random_token(32)),
        }
    }

    #[must_use]
    pub fn token(&self) -> &str {
        &self.token
    }

    #[must_use]
    pub fn csrf_token(&self) -> &str {
        &self.csrf_token
    }

    #[must_use]
    pub fn token_digest(&self) -> [u8; 32] {
        Sha256::digest(self.token.as_bytes()).into()
    }

    #[must_use]
    pub fn csrf_digest(&self) -> [u8; 32] {
        Sha256::digest(self.csrf_token.as_bytes()).into()
    }

    #[must_use]
    pub fn digest_token(token: &str) -> [u8; 32] {
        Sha256::digest(token.as_bytes()).into()
    }

    #[must_use]
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

fn random_token(size: usize) -> String {
    let mut bytes = Zeroizing::new(vec![0_u8; size]);
    rand::rng().fill_bytes(&mut bytes);
    URL_SAFE_NO_PAD.encode(&bytes)
}

#[must_use]
pub fn constant_time_eq(left: &[u8], right: &[u8]) -> bool {
    if left.len() != right.len() {
        return false;
    }
    left.iter()
        .zip(right)
        .fold(0_u8, |difference, (a, b)| difference | (a ^ b))
        == 0
}
