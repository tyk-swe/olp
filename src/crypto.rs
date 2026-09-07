//! Cryptographic primitives and master-key rotation for durable storage.
//!
//! Each child module owns one security concern so callers can depend on an
//! intentionally small surface without weakening secret redaction or
//! zeroization guarantees.

use thiserror::Error;

#[derive(Debug, Error)]
pub enum Error {
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

pub mod aad;

pub mod envelope;

pub mod key_material;

pub mod password;

pub mod rotation;

pub mod secret_files;

pub mod session_material;

#[cfg(test)]
pub mod tests;
