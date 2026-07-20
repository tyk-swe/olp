use std::path::Path;

use olp_storage::{KeyHasher, MasterKey, MasterKeyEncryptionStatus, PgStore};
use zeroize::Zeroizing;

use super::{AppResult, config::DatabaseArgs};

pub(super) async fn connect_store(args: &DatabaseArgs) -> AppResult<PgStore> {
    Ok(PgStore::connect(&args.database_url, args.database_max_connections).await?)
}

pub(super) async fn load_key_hasher(path: &Path) -> AppResult<KeyHasher> {
    let encoded = Zeroizing::new(tokio::fs::read_to_string(path).await?);
    Ok(KeyHasher::from_base64(&encoded)?)
}

pub(super) async fn load_bootstrap_token_digest(
    path: &Path,
    hasher: &KeyHasher,
) -> AppResult<[u8; 32]> {
    let encoded = Zeroizing::new(tokio::fs::read_to_string(path).await?);
    Ok(hasher.bootstrap_token_digest_from_base64(&encoded)?)
}

pub(super) async fn load_master_key(path: &Path) -> AppResult<MasterKey> {
    let encoded = Zeroizing::new(tokio::fs::read_to_string(path).await?);
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

#[cfg(unix)]
pub(crate) async fn check_secret_permissions(path: &Path) -> AppResult<()> {
    use std::os::unix::fs::PermissionsExt;
    let mode = tokio::fs::metadata(path).await?.permissions().mode() & 0o777;
    if mode & 0o007 != 0 {
        return Err(std::io::Error::other(format!(
            "{} must not be accessible by other users",
            path.display()
        ))
        .into());
    }
    Ok(())
}

#[cfg(not(unix))]
pub(crate) async fn check_secret_permissions(path: &Path) -> AppResult<()> {
    tokio::fs::metadata(path).await?;
    Ok(())
}
