use std::path::Path;

use crate::application::secret_files::read_secret_file;
use olp_db::{
    security::envelope::MasterKey, security::key_material::AuthHmacKey,
    security::rotation::MasterKeyEncryptionStatus, store::Store,
};

use super::{AppResult, config::DatabaseArgs};

pub(super) async fn connect_store(args: &DatabaseArgs) -> AppResult<Store> {
    Ok(Store::connect(&args.database_url, args.database_max_connections).await?)
}

pub(super) async fn load_auth_hmac_key(path: &Path) -> AppResult<AuthHmacKey> {
    let encoded = read_secret_file(path).await?;
    Ok(AuthHmacKey::from_base64(&encoded)?)
}

pub(super) async fn load_bootstrap_token_digest(
    path: &Path,
    auth_hmac_key: &AuthHmacKey,
) -> AppResult<[u8; 32]> {
    let encoded = read_secret_file(path).await?;
    Ok(auth_hmac_key.bootstrap_token_digest_from_base64(&encoded)?)
}

pub(super) async fn load_master_key(path: &Path) -> AppResult<MasterKey> {
    let encoded = read_secret_file(path).await?;
    Ok(MasterKey::from_file_contents(&encoded)?)
}

pub(super) fn ensure_keyring_covers_references(
    master_key: &MasterKey,
    status: &MasterKeyEncryptionStatus,
) -> AppResult<()> {
    if let Some(reference) = status
        .references
        .iter()
        .find(|reference| !master_key.contains_version(reference.key_version))
    {
        return Err(std::io::Error::other(format!(
            "mounted master-key keyring is missing referenced version {}",
            reference.key_version
        ))
        .into());
    }
    Ok(())
}
