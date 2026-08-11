use std::fmt;

use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use rand::RngCore;
use sha2::{Digest, Sha256};
use zeroize::Zeroizing;

struct TokenMaterial(Zeroizing<String>);

pub struct SessionMaterial {
    token: TokenMaterial,
    csrf_token: TokenMaterial,
}

/// One-time, purpose-bound proof of recent authentication. Only its SHA-256
/// digest is stored on the exact session that requested the proof.
pub struct RecentAuthMaterial {
    token: TokenMaterial,
}

/// Replacement CSRF bearer used when an otherwise valid session has lost or
/// corrupted its readable CSRF cookie.
pub struct CsrfMaterial {
    token: TokenMaterial,
}

/// One-time invitation bearer material. Only its SHA-256 digest is persisted;
/// the plaintext is returned by the create-invitation API exactly once.
pub struct InvitationMaterial {
    token: TokenMaterial,
}

impl TokenMaterial {
    fn generate() -> Self {
        Self(random_token())
    }

    fn token(&self) -> &str {
        &self.0
    }

    fn digest(&self) -> [u8; 32] {
        token_digest(self.token())
    }
}

macro_rules! impl_token_material {
    ($name:ident $(, $digest_token:ident)?) => {
        impl $name {
            #[must_use]
            pub fn generate() -> Self {
                Self {
                    token: TokenMaterial::generate(),
                }
            }

            #[must_use]
            pub fn token(&self) -> &str {
                self.token.token()
            }

            #[must_use]
            pub fn token_digest(&self) -> [u8; 32] {
                self.token.digest()
            }

            $(
                #[must_use]
                pub fn $digest_token(token: &str) -> [u8; 32] {
                    token_digest(token)
                }
            )?
        }

        impl fmt::Debug for $name {
            fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
                formatter.write_str(concat!(stringify!($name), "([REDACTED])"))
            }
        }
    };
}

impl_token_material!(InvitationMaterial, digest_token);
impl_token_material!(RecentAuthMaterial, digest_token);
impl_token_material!(CsrfMaterial);

impl SessionMaterial {
    #[must_use]
    pub fn generate() -> Self {
        Self {
            token: TokenMaterial::generate(),
            csrf_token: TokenMaterial::generate(),
        }
    }

    #[must_use]
    pub fn token(&self) -> &str {
        self.token.token()
    }

    #[must_use]
    pub fn csrf_token(&self) -> &str {
        self.csrf_token.token()
    }

    #[must_use]
    pub fn token_digest(&self) -> [u8; 32] {
        self.token.digest()
    }

    #[must_use]
    pub fn csrf_digest(&self) -> [u8; 32] {
        self.csrf_token.digest()
    }

    #[must_use]
    pub fn digest_token(token: &str) -> [u8; 32] {
        token_digest(token)
    }

    #[must_use]
    pub fn verify_csrf(token: &str, expected_digest: &[u8]) -> bool {
        constant_time_eq(&token_digest(token), expected_digest)
    }
}

impl fmt::Debug for SessionMaterial {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("SessionMaterial([REDACTED])")
    }
}

pub(crate) fn token_digest(token: &str) -> [u8; 32] {
    Sha256::digest(token.as_bytes()).into()
}

pub(crate) fn random_token() -> Zeroizing<String> {
    let mut bytes = Zeroizing::new([0_u8; 32]);
    rand::rng().fill_bytes(&mut *bytes);
    Zeroizing::new(URL_SAFE_NO_PAD.encode(bytes.as_slice()))
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
