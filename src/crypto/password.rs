use argon2::Algorithm;
use argon2::Argon2;
use argon2::Params;
use argon2::Version;
use argon2::password_hash::PasswordHasher;
use argon2::password_hash::PasswordVerifier;
use argon2::password_hash::phc::PasswordHash;
use rand::Rng;
use zeroize::Zeroizing;

use crate::crypto::Error;

const SALT_BYTES: usize = 16;

pub fn hash(password: &str) -> Result<String, Error> {
    let params = Params::new(19_456, 2, 1, Some(32)).map_err(|_| Error::PasswordHash)?;
    let argon2 = Argon2::new(Algorithm::Argon2id, Version::V0x13, params);
    let mut salt_bytes = Zeroizing::new([0_u8; SALT_BYTES]);
    rand::rng().fill_bytes(salt_bytes.as_mut());
    argon2
        .hash_password_with_salt(password.as_bytes(), salt_bytes.as_ref())
        .map(|hash| hash.to_string())
        .map_err(|_| Error::PasswordHash)
}

#[must_use]
pub fn verify(password: &str, encoded: &str) -> bool {
    let Ok(hash) = PasswordHash::new(encoded) else {
        return false;
    };
    Argon2::default()
        .verify_password(password.as_bytes(), &hash)
        .is_ok()
}
