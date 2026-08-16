use argon2::{
    Algorithm, Argon2, Params, Version,
    password_hash::{PasswordHash, PasswordHasher, PasswordVerifier, SaltString},
};
use rand::RngCore;
use zeroize::Zeroizing;

use super::Error;

/// `Salt::RECOMMENDED_LENGTH`, the length `SaltString::generate` also uses.
const SALT_BYTES: usize = 16;

pub fn hash(password: &str) -> Result<String, Error> {
    let params = Params::new(19_456, 2, 1, Some(32)).map_err(|_| Error::PasswordHash)?;
    let argon2 = Argon2::new(Algorithm::Argon2id, Version::V0x13, params);
    // argon2 0.5 re-exports rand_core 0.6, whose `OsRng` is only present when
    // some other crate happens to enable its `getrandom` feature. Seeding from
    // the same CSPRNG the rest of this module uses keeps the salt independent
    // of that unification accident.
    let mut salt_bytes = Zeroizing::new([0_u8; SALT_BYTES]);
    rand::rng().fill_bytes(salt_bytes.as_mut());
    let salt = SaltString::encode_b64(salt_bytes.as_ref()).map_err(|_| Error::PasswordHash)?;
    argon2
        .hash_password(password.as_bytes(), &salt)
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
