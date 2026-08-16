use std::{collections::BTreeMap, fmt};

use aes_gcm::{
    Aes256Gcm, Nonce,
    aead::{Aead, KeyInit, Payload},
};
use rand::RngCore;
use serde::Deserialize;
use zeroize::{Zeroize, Zeroizing};

use super::{Error, key_material::decode_key};

const NONCE_BYTES: usize = 12;

/// A rotatable AES-256-GCM master key loaded from a mounted secret file.
pub struct MasterKey {
    active_version: u32,
    keys: BTreeMap<u32, [u8; 32]>,
}

impl MasterKey {
    #[must_use]
    pub fn new(version: u32, bytes: [u8; 32]) -> Self {
        Self {
            active_version: version,
            keys: BTreeMap::from([(version, bytes)]),
        }
    }

    /// Loads either the legacy single-key base64 format (version 1) or a
    /// versioned JSON keyring. The active key encrypts new values; retained
    /// versions are decrypt-only and allow zero-downtime rotation.
    pub fn from_file_contents(contents: &str) -> Result<Self, Error> {
        let trimmed = contents.trim();
        if !trimmed.starts_with('{') {
            return Ok(Self::new(1, decode_key(trimmed)?));
        }
        let mut document: MasterKeyFile =
            serde_json::from_str(trimmed).map_err(|_| Error::InvalidMasterKeyFile)?;
        if document.active_version == 0 || document.keys.is_empty() || document.keys.len() > 32 {
            document.zeroize();
            return Err(Error::InvalidMasterKeyFile);
        }
        let mut keys = BTreeMap::new();
        for entry in &mut document.keys {
            if entry.version == 0 || keys.contains_key(&entry.version) {
                document.zeroize();
                zeroize_key_map(&mut keys);
                return Err(Error::InvalidMasterKeyVersion);
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
            return Err(Error::MissingActiveMasterKey);
        }
        let active_version = document.active_version;
        document.zeroize();
        Ok(Self {
            active_version,
            keys,
        })
    }

    #[must_use]
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

    pub fn seal(&self, plaintext: &[u8], aad: &[u8]) -> Result<EncryptedSecret, Error> {
        let bytes = self
            .keys
            .get(&self.active_version)
            .ok_or(Error::MissingActiveMasterKey)?;
        let cipher = Aes256Gcm::new_from_slice(bytes).map_err(|_| Error::InvalidMasterKey)?;
        let mut nonce = [0_u8; NONCE_BYTES];
        rand::rng().fill_bytes(&mut nonce);
        let ciphertext = cipher
            .encrypt(
                &Nonce::from(nonce),
                Payload {
                    msg: plaintext,
                    aad,
                },
            )
            .map_err(|_| Error::Encryption)?;

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
    ) -> Result<Zeroizing<Vec<u8>>, Error> {
        let bytes = self
            .keys
            .get(&encrypted.key_version)
            .ok_or(Error::Decryption)?;
        let cipher = Aes256Gcm::new_from_slice(bytes).map_err(|_| Error::InvalidMasterKey)?;
        let plaintext = cipher
            .decrypt(
                &Nonce::from(encrypted.nonce),
                Payload {
                    msg: &encrypted.ciphertext,
                    aad,
                },
            )
            .map_err(|_| Error::Decryption)?;
        Ok(Zeroizing::new(plaintext))
    }

    /// Authenticates and decrypts with the envelope's referenced version, then
    /// immediately re-encrypts with the active version. Plaintext remains in
    /// zeroizing memory and is never formatted or returned to callers.
    pub fn reseal(
        &self,
        encrypted: &EncryptedSecret,
        aad: &[u8],
    ) -> Result<EncryptedSecret, Error> {
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
