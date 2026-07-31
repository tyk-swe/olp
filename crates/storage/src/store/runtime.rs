use olp_domain::RuntimeSnapshot;
use sha2::{Digest, Sha256};
use uuid::Uuid;

use super::{PersistenceError, PgStore, PublishedRuntimeRelease};

impl PgStore {
    /// Returns a page of verified releases between the supplied sequence bounds.
    /// Corrupt rows do not consume the requested page size.
    pub async fn valid_runtime_releases_before(
        &self,
        limit: u16,
        installed_sequence: Option<u64>,
        before_sequence: Option<i64>,
    ) -> Result<Vec<PublishedRuntimeRelease>, PersistenceError> {
        let installed_sequence = installed_sequence
            .map(i64::try_from)
            .transpose()
            .map_err(|_| PersistenceError::CorruptRelease)?;
        let page_size = usize::from(limit.clamp(1, 100));
        let mut before_sequence = before_sequence;
        let mut releases = Vec::with_capacity(page_size);
        while releases.len() < page_size {
            let rows = sqlx::query!(
                "SELECT id, sequence, compiled_release, release_sha256, created_at \
                 FROM runtime_generations \
                 WHERE ($1::bigint IS NULL OR sequence > $1) \
                   AND ($2::bigint IS NULL OR sequence < $2) \
                 ORDER BY sequence DESC LIMIT $3",
                installed_sequence,
                before_sequence,
                i64::try_from(page_size - releases.len())
                    .map_err(|_| PersistenceError::CorruptRelease)?
            )
            .fetch_all(&self.pool)
            .await?;
            let exhausted = rows.len() < page_size - releases.len();
            for row in rows {
                let payload: Vec<u8> = row.compiled_release;
                let stored_sha: Vec<u8> = row.release_sha256;
                let generation_id: Uuid = row.id;
                let sequence: i64 = row.sequence;
                before_sequence = Some(sequence);
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
                releases.push(PublishedRuntimeRelease {
                    generation_id,
                    sequence,
                    payload,
                    payload_sha256: actual_sha,
                    created_at: row.created_at,
                });
            }
            if exhausted {
                break;
            }
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
