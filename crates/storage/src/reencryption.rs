use std::fmt;

use sqlx::Row;
use thiserror::Error;
use uuid::Uuid;

use crate::{
    EncryptedSecret, MasterKey, PgStore, SecurityError, credential_aad, idempotency_replay_aad,
    oidc_client_secret_aad, oidc_flow_payload_aad,
};

const MAX_BATCH_SIZE: u16 = 1_000;

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum EncryptedTable {
    ProviderCredentialVersions,
    OidcConfigurations,
    OidcAuthorizationFlows,
    IdempotencyRecords,
}

impl EncryptedTable {
    pub const ALL: [Self; 4] = [
        Self::ProviderCredentialVersions,
        Self::OidcConfigurations,
        Self::OidcAuthorizationFlows,
        Self::IdempotencyRecords,
    ];

    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::ProviderCredentialVersions => "provider_credential_versions",
            Self::OidcConfigurations => "oidc_configurations",
            Self::OidcAuthorizationFlows => "oidc_authorization_flows",
            Self::IdempotencyRecords => "idempotency_records",
        }
    }
}

impl fmt::Display for EncryptedTable {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.as_str())
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct KeyVersionReference {
    pub table: EncryptedTable,
    pub key_version: u32,
    pub row_count: u64,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct MasterKeyEncryptionStatus {
    pub active_version: u32,
    pub references: Vec<KeyVersionReference>,
}

impl MasterKeyEncryptionStatus {
    #[must_use]
    pub fn total_references(&self) -> u64 {
        self.references
            .iter()
            .fold(0_u64, |total, item| total.saturating_add(item.row_count))
    }

    #[must_use]
    pub fn references_to(&self, version: u32) -> u64 {
        self.references
            .iter()
            .filter(|item| item.key_version == version)
            .fold(0_u64, |total, item| total.saturating_add(item.row_count))
    }

    #[must_use]
    pub fn non_active_references(&self) -> u64 {
        self.references
            .iter()
            .filter(|item| item.key_version != self.active_version)
            .fold(0_u64, |total, item| total.saturating_add(item.row_count))
    }
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct MasterKeyReencryptionBatch {
    pub rows_reencrypted: u64,
    pub by_table: Vec<(EncryptedTable, u64)>,
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct MasterKeyVerification {
    pub rows_verified: u64,
    pub by_table: Vec<(EncryptedTable, u64)>,
}

#[derive(Debug, Error)]
pub enum ReencryptionError {
    #[error("database operation failed")]
    Database(#[from] sqlx::Error),
    #[error("batch size must be between 1 and {MAX_BATCH_SIZE}")]
    InvalidBatchSize,
    #[error("encrypted row metadata is corrupt in {table} ({row_id})")]
    CorruptEnvelope { table: EncryptedTable, row_id: Uuid },
    #[error("encrypted row authentication failed in {table} ({row_id})")]
    Authentication {
        table: EncryptedTable,
        row_id: Uuid,
        #[source]
        source: SecurityError,
    },
    #[error("active master-key version {0} cannot be retired")]
    ActiveVersionRetirement(u32),
    #[error("master-key version {version} still has {references} encrypted row references")]
    RetirementReferencesRemain { version: u32, references: u64 },
    #[error("master-key version {0} is not present in the mounted keyring")]
    MissingKeyVersion(u32),
    #[error("master-key version {0} cannot be represented by the database schema")]
    InvalidDatabaseKeyVersion(u32),
}

impl PgStore {
    pub async fn master_key_encryption_status(
        &self,
        active_version: u32,
    ) -> Result<MasterKeyEncryptionStatus, ReencryptionError> {
        // Every envelope version is stored in a PostgreSQL integer. Reject an
        // unusable active key before even a read-only rehearsal can report a
        // misleadingly healthy rotation state.
        let _ = database_version(active_version)?;
        let rows = sqlx::query(
            "SELECT encrypted_table, key_version, count(*)::bigint AS row_count FROM ( \
               SELECT 'provider_credential_versions'::text AS encrypted_table, \
                      master_key_version AS key_version \
                 FROM provider_credential_versions \
               UNION ALL \
               SELECT 'oidc_configurations', secret_key_version \
                 FROM oidc_configurations WHERE secret_key_version IS NOT NULL \
               UNION ALL \
               SELECT 'oidc_authorization_flows', payload_key_version \
                 FROM oidc_authorization_flows \
               UNION ALL \
               SELECT 'idempotency_records', replay_key_version \
                 FROM idempotency_records WHERE replay_key_version IS NOT NULL \
             ) encrypted_rows \
             GROUP BY encrypted_table, key_version ORDER BY encrypted_table, key_version",
        )
        .fetch_all(self.pool())
        .await?;
        let mut references = Vec::with_capacity(rows.len());
        for row in rows {
            let table_name: String = row.get("encrypted_table");
            let table =
                parse_table(&table_name).ok_or_else(|| ReencryptionError::CorruptEnvelope {
                    table: EncryptedTable::IdempotencyRecords,
                    row_id: Uuid::nil(),
                })?;
            let key_version = u32::try_from(row.get::<i32, _>("key_version")).map_err(|_| {
                ReencryptionError::CorruptEnvelope {
                    table,
                    row_id: Uuid::nil(),
                }
            })?;
            let row_count = u64::try_from(row.get::<i64, _>("row_count")).map_err(|_| {
                ReencryptionError::CorruptEnvelope {
                    table,
                    row_id: Uuid::nil(),
                }
            })?;
            references.push(KeyVersionReference {
                table,
                key_version,
                row_count,
            });
        }
        Ok(MasterKeyEncryptionStatus {
            active_version,
            references,
        })
    }

    pub async fn reencrypt_master_key_batch(
        &self,
        master_key: &MasterKey,
        batch_size: u16,
    ) -> Result<MasterKeyReencryptionBatch, ReencryptionError> {
        validate_batch_size(batch_size)?;
        let mut remaining = u64::from(batch_size);
        let mut report = MasterKeyReencryptionBatch::default();
        for table in EncryptedTable::ALL {
            if remaining == 0 {
                break;
            }
            let updated = match table {
                EncryptedTable::ProviderCredentialVersions => {
                    self.reencrypt_provider_credentials(master_key, remaining)
                        .await?
                }
                EncryptedTable::OidcConfigurations => {
                    self.reencrypt_oidc_configurations(master_key, remaining)
                        .await?
                }
                EncryptedTable::OidcAuthorizationFlows => {
                    self.reencrypt_oidc_flows(master_key, remaining).await?
                }
                EncryptedTable::IdempotencyRecords => {
                    self.reencrypt_idempotency_records(master_key, remaining)
                        .await?
                }
            };
            if updated > 0 {
                report.by_table.push((table, updated));
                report.rows_reencrypted = report.rows_reencrypted.saturating_add(updated);
                remaining = remaining.saturating_sub(updated);
            }
        }
        Ok(report)
    }

    pub async fn verify_master_key_envelopes(
        &self,
        master_key: &MasterKey,
        batch_size: u16,
    ) -> Result<MasterKeyVerification, ReencryptionError> {
        validate_batch_size(batch_size)?;
        let mut report = MasterKeyVerification::default();
        for table in EncryptedTable::ALL {
            let verified = self
                .verify_encrypted_table(master_key, table, batch_size)
                .await?;
            report.rows_verified = report.rows_verified.saturating_add(verified);
            report.by_table.push((table, verified));
        }
        Ok(report)
    }

    pub async fn verify_master_key_retirement(
        &self,
        master_key: &MasterKey,
        version: u32,
        batch_size: u16,
    ) -> Result<MasterKeyVerification, ReencryptionError> {
        if version == master_key.version() {
            return Err(ReencryptionError::ActiveVersionRetirement(version));
        }
        if !master_key.contains_version(version) {
            return Err(ReencryptionError::MissingKeyVersion(version));
        }
        let status = self
            .master_key_encryption_status(master_key.version())
            .await?;
        let references = status.references_to(version);
        if references != 0 {
            return Err(ReencryptionError::RetirementReferencesRemain {
                version,
                references,
            });
        }
        let verified = self
            .verify_master_key_envelopes(master_key, batch_size)
            .await?;
        // A misconfigured old-active replica could create another reference
        // while verification scans. Recheck immediately before declaring the
        // version retireable so that condition fails closed.
        let final_references = self
            .master_key_encryption_status(master_key.version())
            .await?
            .references_to(version);
        if final_references != 0 {
            return Err(ReencryptionError::RetirementReferencesRemain {
                version,
                references: final_references,
            });
        }
        Ok(verified)
    }

    async fn reencrypt_provider_credentials(
        &self,
        master_key: &MasterKey,
        limit: u64,
    ) -> Result<u64, ReencryptionError> {
        let mut transaction = self.pool().begin().await?;
        let rows = sqlx::query(
            "SELECT id, provider_id, version, ciphertext, nonce, master_key_version \
             FROM provider_credential_versions WHERE master_key_version <> $1 \
             ORDER BY id LIMIT $2 FOR UPDATE SKIP LOCKED",
        )
        .bind(database_version(master_key.version())?)
        .bind(i64::try_from(limit).unwrap_or(i64::MAX))
        .fetch_all(&mut *transaction)
        .await?;
        for row in &rows {
            let id: Uuid = row.get("id");
            let provider_id: Uuid = row.get("provider_id");
            let credential_version = u32::try_from(row.get::<i32, _>("version"))
                .map_err(|_| corrupt(EncryptedTable::ProviderCredentialVersions, id))?;
            let encrypted = encrypted_from_row(
                EncryptedTable::ProviderCredentialVersions,
                id,
                row.get("master_key_version"),
                row.get("nonce"),
                row.get("ciphertext"),
            )?;
            let resealed = reseal(
                master_key,
                EncryptedTable::ProviderCredentialVersions,
                id,
                &encrypted,
                &credential_aad(provider_id, id, credential_version),
            )?;
            update_envelope(
                &mut transaction,
                EncryptedTable::ProviderCredentialVersions,
                id,
                encrypted.key_version,
                resealed,
            )
            .await?;
        }
        transaction.commit().await?;
        Ok(u64::try_from(rows.len()).unwrap_or(u64::MAX))
    }

    async fn reencrypt_oidc_configurations(
        &self,
        master_key: &MasterKey,
        limit: u64,
    ) -> Result<u64, ReencryptionError> {
        let mut transaction = self.pool().begin().await?;
        let rows = sqlx::query(
            "SELECT id, encrypted_client_secret, secret_nonce, secret_key_version \
             FROM oidc_configurations \
             WHERE secret_key_version IS NOT NULL AND secret_key_version <> $1 \
             ORDER BY id LIMIT $2 FOR UPDATE SKIP LOCKED",
        )
        .bind(database_version(master_key.version())?)
        .bind(i64::try_from(limit).unwrap_or(i64::MAX))
        .fetch_all(&mut *transaction)
        .await?;
        for row in &rows {
            let id: Uuid = row.get("id");
            let encrypted = encrypted_from_row(
                EncryptedTable::OidcConfigurations,
                id,
                row.get("secret_key_version"),
                row.get("secret_nonce"),
                row.get("encrypted_client_secret"),
            )?;
            let resealed = reseal(
                master_key,
                EncryptedTable::OidcConfigurations,
                id,
                &encrypted,
                &oidc_client_secret_aad(id),
            )?;
            update_envelope(
                &mut transaction,
                EncryptedTable::OidcConfigurations,
                id,
                encrypted.key_version,
                resealed,
            )
            .await?;
        }
        transaction.commit().await?;
        Ok(u64::try_from(rows.len()).unwrap_or(u64::MAX))
    }

    async fn reencrypt_oidc_flows(
        &self,
        master_key: &MasterKey,
        limit: u64,
    ) -> Result<u64, ReencryptionError> {
        let mut transaction = self.pool().begin().await?;
        let rows = sqlx::query(
            "SELECT id, encrypted_payload, payload_nonce, payload_key_version \
             FROM oidc_authorization_flows WHERE payload_key_version <> $1 \
             ORDER BY id LIMIT $2 FOR UPDATE SKIP LOCKED",
        )
        .bind(database_version(master_key.version())?)
        .bind(i64::try_from(limit).unwrap_or(i64::MAX))
        .fetch_all(&mut *transaction)
        .await?;
        for row in &rows {
            let id: Uuid = row.get("id");
            let encrypted = encrypted_from_row(
                EncryptedTable::OidcAuthorizationFlows,
                id,
                row.get("payload_key_version"),
                row.get("payload_nonce"),
                row.get("encrypted_payload"),
            )?;
            let resealed = reseal(
                master_key,
                EncryptedTable::OidcAuthorizationFlows,
                id,
                &encrypted,
                &oidc_flow_payload_aad(id),
            )?;
            update_envelope(
                &mut transaction,
                EncryptedTable::OidcAuthorizationFlows,
                id,
                encrypted.key_version,
                resealed,
            )
            .await?;
        }
        transaction.commit().await?;
        Ok(u64::try_from(rows.len()).unwrap_or(u64::MAX))
    }

    async fn reencrypt_idempotency_records(
        &self,
        master_key: &MasterKey,
        limit: u64,
    ) -> Result<u64, ReencryptionError> {
        let mut transaction = self.pool().begin().await?;
        let rows = sqlx::query(
            "SELECT id, actor_user_id, operation, idempotency_key, replay_ciphertext, \
                    replay_nonce, replay_key_version \
             FROM idempotency_records \
             WHERE replay_key_version IS NOT NULL AND replay_key_version <> $1 \
             ORDER BY id LIMIT $2 FOR UPDATE SKIP LOCKED",
        )
        .bind(database_version(master_key.version())?)
        .bind(i64::try_from(limit).unwrap_or(i64::MAX))
        .fetch_all(&mut *transaction)
        .await?;
        for row in &rows {
            let id: Uuid = row.get("id");
            let actor: Uuid = row.get("actor_user_id");
            let operation: String = row.get("operation");
            let key: String = row.get("idempotency_key");
            let encrypted = encrypted_from_row(
                EncryptedTable::IdempotencyRecords,
                id,
                row.get("replay_key_version"),
                row.get("replay_nonce"),
                row.get("replay_ciphertext"),
            )?;
            let resealed = reseal(
                master_key,
                EncryptedTable::IdempotencyRecords,
                id,
                &encrypted,
                &idempotency_replay_aad(actor, &operation, &key),
            )?;
            update_envelope(
                &mut transaction,
                EncryptedTable::IdempotencyRecords,
                id,
                encrypted.key_version,
                resealed,
            )
            .await?;
        }
        transaction.commit().await?;
        Ok(u64::try_from(rows.len()).unwrap_or(u64::MAX))
    }

    async fn verify_encrypted_table(
        &self,
        master_key: &MasterKey,
        table: EncryptedTable,
        batch_size: u16,
    ) -> Result<u64, ReencryptionError> {
        let mut cursor: Option<Uuid> = None;
        let mut verified = 0_u64;
        loop {
            let rows = match table {
                EncryptedTable::ProviderCredentialVersions => {
                    sqlx::query(
                        "SELECT id, provider_id, version, ciphertext AS encrypted, nonce, \
                            master_key_version AS key_version \
                     FROM provider_credential_versions WHERE ($1::uuid IS NULL OR id > $1) \
                     ORDER BY id LIMIT $2",
                    )
                    .bind(cursor)
                    .bind(i64::from(batch_size))
                    .fetch_all(self.pool())
                    .await?
                }
                EncryptedTable::OidcConfigurations => {
                    sqlx::query(
                        "SELECT id, NULL::uuid AS provider_id, NULL::integer AS version, \
                            encrypted_client_secret AS encrypted, secret_nonce AS nonce, \
                            secret_key_version AS key_version \
                     FROM oidc_configurations WHERE secret_key_version IS NOT NULL \
                       AND ($1::uuid IS NULL OR id > $1) ORDER BY id LIMIT $2",
                    )
                    .bind(cursor)
                    .bind(i64::from(batch_size))
                    .fetch_all(self.pool())
                    .await?
                }
                EncryptedTable::OidcAuthorizationFlows => {
                    sqlx::query(
                        "SELECT id, NULL::uuid AS provider_id, NULL::integer AS version, \
                            encrypted_payload AS encrypted, payload_nonce AS nonce, \
                            payload_key_version AS key_version \
                     FROM oidc_authorization_flows WHERE ($1::uuid IS NULL OR id > $1) \
                     ORDER BY id LIMIT $2",
                    )
                    .bind(cursor)
                    .bind(i64::from(batch_size))
                    .fetch_all(self.pool())
                    .await?
                }
                EncryptedTable::IdempotencyRecords => {
                    sqlx::query(
                        "SELECT id, NULL::uuid AS provider_id, NULL::integer AS version, \
                            replay_ciphertext AS encrypted, replay_nonce AS nonce, \
                            replay_key_version AS key_version, actor_user_id, operation, \
                            idempotency_key \
                     FROM idempotency_records WHERE replay_key_version IS NOT NULL \
                       AND ($1::uuid IS NULL OR id > $1) ORDER BY id LIMIT $2",
                    )
                    .bind(cursor)
                    .bind(i64::from(batch_size))
                    .fetch_all(self.pool())
                    .await?
                }
            };
            if rows.is_empty() {
                break;
            }
            for row in &rows {
                let id: Uuid = row.get("id");
                let encrypted = encrypted_from_row(
                    table,
                    id,
                    row.get("key_version"),
                    row.get("nonce"),
                    row.get("encrypted"),
                )?;
                let aad = match table {
                    EncryptedTable::ProviderCredentialVersions => {
                        let provider_id: Uuid = row.get("provider_id");
                        let version = u32::try_from(row.get::<i32, _>("version"))
                            .map_err(|_| corrupt(table, id))?;
                        credential_aad(provider_id, id, version)
                    }
                    EncryptedTable::OidcConfigurations => oidc_client_secret_aad(id),
                    EncryptedTable::OidcAuthorizationFlows => oidc_flow_payload_aad(id),
                    EncryptedTable::IdempotencyRecords => idempotency_replay_aad(
                        row.get("actor_user_id"),
                        row.get::<String, _>("operation").as_str(),
                        row.get::<String, _>("idempotency_key").as_str(),
                    ),
                };
                master_key.open(&encrypted, &aad).map_err(|source| {
                    ReencryptionError::Authentication {
                        table,
                        row_id: id,
                        source,
                    }
                })?;
                cursor = Some(id);
                verified = verified.saturating_add(1);
            }
            if rows.len() < usize::from(batch_size) {
                break;
            }
        }
        Ok(verified)
    }
}

fn parse_table(value: &str) -> Option<EncryptedTable> {
    EncryptedTable::ALL
        .into_iter()
        .find(|table| table.as_str() == value)
}

fn validate_batch_size(batch_size: u16) -> Result<(), ReencryptionError> {
    if batch_size == 0 || batch_size > MAX_BATCH_SIZE {
        Err(ReencryptionError::InvalidBatchSize)
    } else {
        Ok(())
    }
}

fn database_version(version: u32) -> Result<i32, ReencryptionError> {
    i32::try_from(version).map_err(|_| ReencryptionError::InvalidDatabaseKeyVersion(version))
}

fn encrypted_from_row(
    table: EncryptedTable,
    row_id: Uuid,
    key_version: i32,
    nonce: Vec<u8>,
    ciphertext: Vec<u8>,
) -> Result<EncryptedSecret, ReencryptionError> {
    let key_version = u32::try_from(key_version).map_err(|_| corrupt(table, row_id))?;
    let nonce = nonce.try_into().map_err(|_| corrupt(table, row_id))?;
    if ciphertext.len() < 16 {
        return Err(corrupt(table, row_id));
    }
    Ok(EncryptedSecret {
        key_version,
        nonce,
        ciphertext,
    })
}

fn reseal(
    master_key: &MasterKey,
    table: EncryptedTable,
    row_id: Uuid,
    encrypted: &EncryptedSecret,
    aad: &[u8],
) -> Result<EncryptedSecret, ReencryptionError> {
    master_key
        .reseal(encrypted, aad)
        .map_err(|source| ReencryptionError::Authentication {
            table,
            row_id,
            source,
        })
}

fn corrupt(table: EncryptedTable, row_id: Uuid) -> ReencryptionError {
    ReencryptionError::CorruptEnvelope { table, row_id }
}

async fn update_envelope(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    table: EncryptedTable,
    row_id: Uuid,
    previous_version: u32,
    encrypted: EncryptedSecret,
) -> Result<(), ReencryptionError> {
    let ciphertext = encrypted.ciphertext;
    let nonce = encrypted.nonce.to_vec();
    let key_version = database_version(encrypted.key_version)?;
    let previous_version = database_version(previous_version)?;
    let updated = match table {
        EncryptedTable::ProviderCredentialVersions => {
            sqlx::query(
                "UPDATE provider_credential_versions \
             SET ciphertext = $1, nonce = $2, master_key_version = $3 \
             WHERE id = $4 AND master_key_version = $5",
            )
            .bind(ciphertext)
            .bind(nonce)
            .bind(key_version)
            .bind(row_id)
            .bind(previous_version)
            .execute(&mut **transaction)
            .await?
        }
        EncryptedTable::OidcConfigurations => {
            sqlx::query(
                "UPDATE oidc_configurations \
             SET encrypted_client_secret = $1, secret_nonce = $2, secret_key_version = $3 \
             WHERE id = $4 AND secret_key_version = $5",
            )
            .bind(ciphertext)
            .bind(nonce)
            .bind(key_version)
            .bind(row_id)
            .bind(previous_version)
            .execute(&mut **transaction)
            .await?
        }
        EncryptedTable::OidcAuthorizationFlows => {
            sqlx::query(
                "UPDATE oidc_authorization_flows \
             SET encrypted_payload = $1, payload_nonce = $2, payload_key_version = $3 \
             WHERE id = $4 AND payload_key_version = $5",
            )
            .bind(ciphertext)
            .bind(nonce)
            .bind(key_version)
            .bind(row_id)
            .bind(previous_version)
            .execute(&mut **transaction)
            .await?
        }
        EncryptedTable::IdempotencyRecords => {
            sqlx::query(
                "UPDATE idempotency_records \
             SET replay_ciphertext = $1, replay_nonce = $2, replay_key_version = $3 \
             WHERE id = $4 AND replay_key_version = $5",
            )
            .bind(ciphertext)
            .bind(nonce)
            .bind(key_version)
            .bind(row_id)
            .bind(previous_version)
            .execute(&mut **transaction)
            .await?
        }
    };
    if updated.rows_affected() != 1 {
        return Err(corrupt(table, row_id));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn encrypted_table_inventory_is_closed_and_complete() {
        assert_eq!(EncryptedTable::ALL.len(), 4);
        assert_eq!(
            EncryptedTable::ALL.map(EncryptedTable::as_str),
            [
                "provider_credential_versions",
                "oidc_configurations",
                "oidc_authorization_flows",
                "idempotency_records",
            ]
        );
    }

    #[test]
    fn status_sums_versions_without_hiding_old_references() {
        let status = MasterKeyEncryptionStatus {
            active_version: 2,
            references: vec![
                KeyVersionReference {
                    table: EncryptedTable::ProviderCredentialVersions,
                    key_version: 1,
                    row_count: 2,
                },
                KeyVersionReference {
                    table: EncryptedTable::OidcConfigurations,
                    key_version: 2,
                    row_count: 1,
                },
            ],
        };
        assert_eq!(status.total_references(), 3);
        assert_eq!(status.references_to(1), 2);
        assert_eq!(status.non_active_references(), 2);
    }

    #[test]
    fn database_key_versions_fail_closed_on_integer_overflow() {
        assert_eq!(database_version(1).unwrap(), 1);
        assert!(matches!(
            database_version(u32::MAX),
            Err(ReencryptionError::InvalidDatabaseKeyVersion(u32::MAX))
        ));
    }
}
