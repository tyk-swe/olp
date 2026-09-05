use olp_db::{security::envelope::MasterKey, security::rotation::MasterKeyEncryptionStatus};
use tracing::info;

use {
    super::{
        AppResult,
        config::{MasterKeyAction, MasterKeyArgs},
        validation::{connect_store, ensure_keyring_covers_references, load_master_key},
    },
    crate::application::secret_files::check_secret_permissions,
};

pub(super) async fn master_key_command(args: MasterKeyArgs) -> AppResult<()> {
    check_secret_permissions(&args.master_key_file).await?;
    let master_key = load_master_key(&args.master_key_file).await?;
    let store = connect_store(&args.database).await?;
    match args.action {
        MasterKeyAction::Status { batch_size } => {
            let status = store
                .master_key_encryption_status(master_key.version())
                .await?;
            report_master_key_status(&master_key, &status);
            ensure_keyring_covers_references(&master_key, &status)?;
            let verified = store
                .verify_master_key_envelopes(&master_key, batch_size)
                .await?;
            info!(
                active_version = master_key.version(),
                rows_verified = verified.rows_verified,
                "master-key envelope status verified"
            );
        }
        MasterKeyAction::Reencrypt {
            batch_size,
            dry_run,
        } => {
            reencrypt_master_key(&store, &master_key, batch_size, dry_run).await?;
        }
        MasterKeyAction::VerifyRetirement {
            version,
            batch_size,
        } => {
            let status = store
                .master_key_encryption_status(master_key.version())
                .await?;
            report_master_key_status(&master_key, &status);
            ensure_keyring_covers_references(&master_key, &status)?;
            let verified = store
                .verify_master_key_retirement(&master_key, version, batch_size)
                .await?;
            info!(
                active_version = master_key.version(),
                retirement_version = version,
                rows_verified = verified.rows_verified,
                "master-key version has zero references and is safe to remove after all replicas use the active keyring"
            );
        }
    }
    Ok(())
}

fn report_master_key_status(master_key: &MasterKey, status: &MasterKeyEncryptionStatus) {
    let available_versions = master_key.versions().collect::<Vec<_>>();
    info!(
        active_version = master_key.version(),
        available_versions = ?available_versions,
        total_encrypted_rows = status.total_references(),
        non_active_references = status.non_active_references(),
        "master-key reference status"
    );
    for reference in &status.references {
        info!(
            encrypted_table = reference.table.as_str(),
            key_version = reference.key_version,
            row_count = reference.row_count,
            "master-key references"
        );
    }
}

async fn reencrypt_master_key(
    store: &olp_db::store::Store,
    master_key: &MasterKey,
    batch_size: u16,
    dry_run: bool,
) -> AppResult<()> {
    let initial = store
        .master_key_encryption_status(master_key.version())
        .await?;
    report_master_key_status(master_key, &initial);
    ensure_keyring_covers_references(master_key, &initial)?;
    if dry_run {
        let verified = store
            .verify_master_key_envelopes(master_key, batch_size)
            .await?;
        info!(
            active_version = master_key.version(),
            rows_verified = verified.rows_verified,
            rows_requiring_reencryption = initial.non_active_references(),
            "master-key re-encryption dry run completed without writes"
        );
        return Ok(());
    }
    let mut total_reencrypted = 0_u64;
    loop {
        let status = store
            .master_key_encryption_status(master_key.version())
            .await?;
        ensure_keyring_covers_references(master_key, &status)?;
        if status.non_active_references() == 0 {
            break;
        }
        let batch = store
            .reencrypt_master_key_batch(master_key, batch_size)
            .await?;
        if batch.rows_reencrypted == 0 {
            return Err(std::io::Error::other(
                "master-key re-encryption made no progress while old references remain",
            )
            .into());
        }
        total_reencrypted = total_reencrypted.saturating_add(batch.rows_reencrypted);
        for (table, rows) in batch.by_table {
            info!(
                active_version = master_key.version(),
                encrypted_table = table.as_str(),
                rows_reencrypted = rows,
                total_reencrypted,
                "master-key re-encryption batch committed"
            );
        }
    }
    let verified = store
        .verify_master_key_envelopes(master_key, batch_size)
        .await?;
    let final_status = store
        .master_key_encryption_status(master_key.version())
        .await?;
    report_master_key_status(master_key, &final_status);
    if final_status.non_active_references() != 0 {
        return Err(std::io::Error::other(
                    "non-active master-key references appeared during final verification; confirm every replica uses the new active version and rerun",
                )
                .into());
    }
    info!(
        active_version = master_key.version(),
        rows_reencrypted = total_reencrypted,
        rows_verified = verified.rows_verified,
        "master-key re-encryption completed"
    );

    Ok(())
}
