use olp_domain::RuntimeSnapshot;
use sha2::{Digest, Sha256};
use sqlx::Row;
use uuid::Uuid;

use super::{PersistenceError, PgStore, PublishedRelease};

impl PgStore {
    /// Returns newest verified releases, skipping and visibly logging corrupt
    /// envelopes so a replacement gateway can try its previous durable LKG.
    pub async fn recent_valid_releases(
        &self,
        limit: u16,
    ) -> Result<Vec<PublishedRelease>, PersistenceError> {
        self.recent_valid_releases_after(limit, None).await
    }

    /// Returns verified releases newer than the supplied installed sequence.
    /// Pollers use this to avoid decoding unchanged immutable snapshots.
    pub async fn recent_valid_releases_after(
        &self,
        limit: u16,
        installed_sequence: Option<u64>,
    ) -> Result<Vec<PublishedRelease>, PersistenceError> {
        let installed_sequence = installed_sequence
            .map(i64::try_from)
            .transpose()
            .map_err(|_| PersistenceError::CorruptRelease)?;
        let rows = sqlx::query(
            "SELECT id, sequence, compiled_release, release_sha256, created_at \
             FROM runtime_generations \
             WHERE ($1::bigint IS NULL OR sequence > $1) \
             ORDER BY sequence DESC LIMIT $2",
        )
        .bind(installed_sequence)
        .bind(i64::from(limit.clamp(1, 100)))
        .fetch_all(&self.pool)
        .await?;
        let mut releases = Vec::with_capacity(rows.len());
        for row in rows {
            let payload: Vec<u8> = row.get("compiled_release");
            let stored_sha: Vec<u8> = row.get("release_sha256");
            let generation_id: Uuid = row.get("id");
            let sequence: i64 = row.get("sequence");
            let actual_sha: [u8; 32] = Sha256::digest(&payload).into();
            if stored_sha.as_slice() != actual_sha
                || verify_release_envelope(&payload, generation_id, sequence).is_err()
            {
                tracing::error!(
                    %generation_id,
                    sequence,
                    "skipping corrupt runtime release while searching for last-known-good"
                );
                continue;
            }
            releases.push(PublishedRelease {
                generation_id,
                sequence,
                payload,
                sha256: actual_sha,
                created_at: row.get("created_at"),
            });
        }
        Ok(releases)
    }
}

pub(super) fn verify_release_envelope(
    payload: &[u8],
    generation_id: Uuid,
    sequence: i64,
) -> Result<(), PersistenceError> {
    if generation_id.get_version_num() != 7 {
        return Err(PersistenceError::CorruptRelease);
    }
    let ordinal = u64::try_from(sequence).map_err(|_| PersistenceError::CorruptRelease)?;
    let snapshot = RuntimeSnapshot::from_persisted_slice(payload)
        .map_err(|_| PersistenceError::CorruptRelease)?;
    if snapshot.generation.id.as_uuid() != generation_id
        || snapshot.generation.ordinal != ordinal
        || snapshot.validate().is_err()
    {
        return Err(PersistenceError::CorruptRelease);
    }
    Ok(())
}
