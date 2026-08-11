//! Cryptographic primitives and master-key rotation for durable storage.
//!
//! Each child module owns one security concern so callers can depend on an
//! intentionally small surface without weakening secret redaction or
//! zeroization guarantees.

mod aad;
mod envelope;
mod key_material;
mod password;
mod rotation;
mod session_material;

use thiserror::Error;

pub use aad::{
    credential_aad, idempotency_replay_aad, idempotency_replay_scope, oidc_client_secret_aad,
    oidc_flow_payload_aad,
};
pub use envelope::{EncryptedSecret, MasterKey};
pub use key_material::{ApiKeyMaterial, AuthHmacKey, ParsedApiKey};
pub use password::{hash_password, verify_password};
pub use rotation::{
    EncryptedTable, KeyVersionReference, MasterKeyEncryptionStatus, MasterKeyReencryptionBatch,
    MasterKeyVerification, ReencryptionError,
};
pub use session_material::{
    CsrfMaterial, InvitationMaterial, RecentAuthMaterial, SessionMaterial, constant_time_eq,
};
pub(crate) use session_material::{random_token, token_digest};

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

#[cfg(test)]
mod tests;
