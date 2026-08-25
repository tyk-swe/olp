use olp_engine::domain::routing::snapshot::Snapshot;
use sha2::{Digest, Sha256};
use uuid::Uuid;

use crate::{error::Error, store::Store};

use super::PublishedRuntimeRelease;

/// Upper bound on rows examined while searching for verified releases. A run
/// of corrupt rows must not hide an intact older release, but neither should
/// one call walk an unbounded history.
const RUNTIME_RELEASE_SCAN_LIMIT: usize = 1_024;

impl Store {
    /// Returns verified releases newer than the supplied installed sequence.
    /// Pollers use this to avoid decoding unchanged immutable snapshots.
    ///
    /// Verification is a Rust-side check of the stored digest and the release
    /// envelope, so the scan pages descending and keeps reading past corrupt
    /// rows. Truncating to `limit` in SQL first would let a run of `limit`
    /// corrupt releases hide every intact one behind it, defeating the
    /// last-known-good fallback.
    pub async fn recent_valid_runtime_releases_after(
        &self,
        limit: u16,
        installed_sequence: Option<u64>,
    ) -> Result<Vec<PublishedRuntimeRelease>, Error> {
        let installed_sequence = installed_sequence
            .map(i64::try_from)
            .transpose()
            .map_err(|_| Error::CorruptRelease)?;
        let wanted = usize::from(limit.clamp(1, 100));
        let page_size = i64::try_from(wanted).unwrap_or(100);
        let mut releases = Vec::with_capacity(wanted);
        let mut scanned = 0_usize;
        let mut before_sequence: Option<i64> = None;
        while releases.len() < wanted && scanned < RUNTIME_RELEASE_SCAN_LIMIT {
            let rows = sqlx::query!(
                "SELECT id, sequence, compiled_release, release_sha256, created_at \
                 FROM runtime_generations \
                 WHERE ($1::bigint IS NULL OR sequence > $1) \
                   AND ($2::bigint IS NULL OR sequence < $2) \
                 ORDER BY sequence DESC LIMIT $3",
                installed_sequence,
                before_sequence,
                page_size
            )
            .fetch_all(self.pool())
            .await?;
            let exhausted = rows.len() < wanted;
            for row in rows {
                scanned += 1;
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
                if releases.len() == wanted {
                    break;
                }
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
) -> Result<(), Error> {
    if generation_id.get_version_num() != 7 {
        return Err(Error::CorruptRelease);
    }
    let ordinal = u64::try_from(sequence).map_err(|_| Error::CorruptRelease)?;
    let snapshot = Snapshot::from_persisted_slice(payload).map_err(|_| Error::CorruptRelease)?;
    if snapshot.generation.id.as_uuid() != generation_id
        || snapshot.generation.ordinal != ordinal
        || snapshot.validate().is_err()
    {
        return Err(Error::CorruptRelease);
    }
    Ok(())
}
