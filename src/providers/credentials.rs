use crate::access::audit_events::record_success;
use crate::crypto::envelope::EncryptedSecret;
use crate::database::error::Error as PersistenceError;
use crate::database::idempotency::Outcome;
use crate::database::idempotency::Replayable;
use crate::database::idempotency::ReplayableIdempotencyClaim;
use crate::database::idempotency::Response;
use crate::database::idempotency::claim_idempotency;
use crate::database::idempotency::claim_replayable_idempotency;
use crate::database::idempotency::complete_idempotency;
use crate::database::idempotency::complete_replayable_idempotency;
use crate::database::page::ConfigurationPage;
use crate::database::query::split_page;
use crate::providers::error::Error;
use crate::providers::queries::lock_provider;
use crate::providers::record_validation::checked_limit;
use crate::providers::records::CredentialVersionRecord;
use crate::providers::records::ProviderMutationResult;
use crate::providers::records::RotateCredentialInput;
use crate::providers::records::StoredCredentialSecret;
use crate::runtime::publication::compiler::prepare_runtime_mutation;
use uuid::Uuid;

/// Returns the next candidate version without enforcing an HTTP
/// precondition. The transactional rotation still checks both the ETag and
/// candidate version after claiming idempotency; this allows an identical
/// retry with the original ETag to reach its persisted replay response.
pub async fn next_credential_version_candidate(
    pool: &sqlx::PgPool,
    provider_id: Uuid,
) -> Result<u32, Error> {
    let next_version: i32 = sqlx::query_scalar::<_, i32>(
        "SELECT COALESCE(max(cv.version), 0) + 1 AS \"value\" \
             FROM providers p LEFT JOIN provider_credential_versions cv ON cv.provider_id = p.id \
             WHERE p.id = $1 GROUP BY p.id",
    )
    .bind(provider_id)
    .fetch_optional(pool)
    .await?
    .ok_or(Error::NotFound)?;
    u32::try_from(next_version)
        .map_err(|_| Error::Invalid("credential version overflow".to_owned()))
}

pub async fn rotate_provider_credential<F>(
    pool: &sqlx::PgPool,
    provenance: &crate::database::RequestProvenance,
    provider_id: Uuid,
    input: RotateCredentialInput,
    replay: Replayable<'_>,
    build_response: F,
) -> Result<Outcome<ProviderMutationResult>, Error>
where
    F: FnOnce(&ProviderMutationResult) -> Result<Response, PersistenceError>,
{
    let mut transaction = pool
        .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
        .await?;
    match claim_replayable_idempotency(
        &mut transaction,
        input.actor,
        "provider.rotate_credential",
        &input.idempotency_key,
        replay.request_fingerprint(),
        replay.master_key(),
    )
    .await?
    {
        ReplayableIdempotencyClaim::Execute => {
            prepare_runtime_mutation(&mut transaction).await?;
        }
        ReplayableIdempotencyClaim::Replay(response) => {
            transaction.rollback().await?;
            return Ok(Outcome::Replayed(response));
        }
        ReplayableIdempotencyClaim::Conflict => {
            transaction.rollback().await?;
            return Err(Error::IdempotencyConflict);
        }
        ReplayableIdempotencyClaim::InProgress => {
            transaction.rollback().await?;
            return Err(Error::IdempotencyInProgress);
        }
    }
    let etag = rotate_credential_record(&mut transaction, provider_id, &input).await?;
    record_success(
        &mut *transaction,
        provenance,
        input.actor,
        "provider.rotate_credential",
        "provider",
        provider_id,
    )
    .await?;
    let result = ProviderMutationResult {
        etag,
        release: None,
    };
    let response = build_response(&result)?;
    complete_replayable_idempotency(
        &mut transaction,
        input.actor,
        "provider.rotate_credential",
        &input.idempotency_key,
        replay.request_fingerprint(),
        replay.master_key(),
        &response,
    )
    .await?;
    transaction.commit().await?;
    Ok(Outcome::Executed {
        value: result,
        response,
    })
}

pub async fn list_provider_credentials(
    pool: &sqlx::PgPool,
    provider_id: Uuid,
    cursor: Option<Uuid>,
    limit: i64,
) -> Result<ConfigurationPage<CredentialVersionRecord>, Error> {
    let limit = checked_limit(limit)?;
    let exists: bool = sqlx::query_scalar::<_, bool>(
        "SELECT EXISTS (SELECT 1 FROM providers WHERE id = $1) AS \"value\"",
    )
    .bind(provider_id)
    .fetch_one(pool)
    .await?;
    if !exists {
        return Err(Error::NotFound);
    }
    let before_version: Option<i32> = match cursor {
        Some(cursor) => Some(
            sqlx::query_scalar::<_, i32>(
                "SELECT version FROM provider_credential_versions \
                     WHERE provider_id = $1 AND id = $2",
            )
            .bind(provider_id)
            .bind(cursor)
            .fetch_optional(pool)
            .await?
            .ok_or_else(|| Error::Invalid("credential pagination cursor is invalid".to_owned()))?,
        ),
        None => None,
    };
    let items = sqlx::query_as::<_, ListProviderCredentialsRow>(
        "SELECT cv.id, cv.version, \
                    COALESCE(cv.id = ar.credential_version_id, false) AS \"active\", \
                    COALESCE(p.state = 'draft'::provider_state \
                     AND cv.id = p.active_credential_version_id, false) AS \"draft_selected\", \
                    cv.created_at, cv.revoked_at FROM provider_credential_versions cv \
             JOIN providers p ON p.id = cv.provider_id \
             LEFT JOIN provider_revisions ar ON ar.id = p.active_revision_id \
             WHERE cv.provider_id = $1 \
             AND ($2::int IS NULL OR cv.version < $2) \
             ORDER BY cv.version DESC LIMIT $3",
    )
    .bind(provider_id)
    .bind(before_version)
    .bind(limit + 1)
    .fetch_all(pool)
    .await?
    .into_iter()
    .map(|row| CredentialVersionRecord {
        id: row.id,
        version: row.version,
        active: row.active,
        draft_selected: row.draft_selected,
        created_at: row.created_at,
        revoked_at: row.revoked_at,
    })
    .collect::<Vec<_>>();
    let (items, next_cursor) = split_page(items, limit as usize, |item| item.id);
    Ok(ConfigurationPage { items, next_cursor })
}

pub async fn active_provider_credential_secret(
    pool: &sqlx::PgPool,
    provider_id: Uuid,
) -> Result<StoredCredentialSecret, Error> {
    let row = sqlx::query_as::<_, ActiveProviderCredentialSecretRow>(
        "SELECT cv.id, cv.version, cv.ciphertext, cv.nonce, cv.master_key_version \
             FROM providers p JOIN provider_credential_versions cv \
               ON cv.id = p.active_credential_version_id \
             WHERE p.id = $1 AND cv.revoked_at IS NULL",
    )
    .bind(provider_id)
    .fetch_optional(pool)
    .await?
    .ok_or(Error::NotFound)?;
    let nonce: Vec<u8> = row.nonce;
    let nonce: [u8; 12] = nonce
        .try_into()
        .map_err(|_| Error::Invalid("stored credential nonce is invalid".to_owned()))?;
    let version = u32::try_from(row.version)
        .map_err(|_| Error::Invalid("stored credential version is invalid".to_owned()))?;
    let key_version = u32::try_from(row.master_key_version)
        .map_err(|_| Error::Invalid("stored master-key version is invalid".to_owned()))?;
    Ok(StoredCredentialSecret {
        id: row.id,
        version,
        encrypted: EncryptedSecret {
            key_version,
            nonce,
            ciphertext: row.ciphertext,
        },
    })
}

pub async fn revoke_provider_credential(
    pool: &sqlx::PgPool,
    provenance: &crate::database::RequestProvenance,
    provider_id: Uuid,
    credential_id: Uuid,
    expected_etag: Uuid,
    actor: Uuid,
    idempotency_key: &str,
) -> Result<Uuid, Error> {
    let mut transaction = pool.begin().await?;
    if !claim_idempotency(
        &mut transaction,
        actor,
        "provider.revoke_credential",
        idempotency_key,
    )
    .await?
    {
        return Err(Error::IdempotencyConflict);
    }
    let provider = sqlx::query_as::<_, RevokeProviderCredentialRow>(
        "SELECT p.etag, p.active_credential_version_id, \
                    ar.credential_version_id AS \"activated_credential_version_id\" \
             FROM providers p LEFT JOIN provider_revisions ar ON ar.id = p.active_revision_id \
             WHERE p.id = $1 FOR UPDATE OF p",
    )
    .bind(provider_id)
    .fetch_optional(&mut *transaction)
    .await?
    .ok_or(Error::NotFound)?;
    if provider.etag != expected_etag {
        return Err(Error::PreconditionFailed);
    }
    if provider.active_credential_version_id == Some(credential_id)
        || provider.activated_credential_version_id == Some(credential_id)
    {
        return Err(Error::InUse);
    }
    // Historic jobs carry their immutable provider revision. Even an
    // otherwise inactive credential remains lifecycle authority until
    // every job that used it has a durable deletion tombstone.
    let used_by_live_media_job: bool = sqlx::query_scalar::<_, bool>(
        "SELECT EXISTS (
               SELECT 1 FROM async_media_jobs j
               JOIN provider_revisions pr ON pr.id = j.provider_revision_id
               WHERE j.provider_id = $1 AND j.lifecycle_state <> 'deleted'
                 AND pr.credential_version_id = $2
             ) AS \"value\"",
    )
    .bind(provider_id)
    .bind(credential_id)
    .fetch_one(&mut *transaction)
    .await?;
    if used_by_live_media_job {
        return Err(Error::InUse);
    }
    let result = sqlx::query(
        "UPDATE provider_credential_versions SET revoked_at = COALESCE(revoked_at, now()) \
             WHERE id = $1 AND provider_id = $2",
    )
    .bind(credential_id)
    .bind(provider_id)
    .execute(&mut *transaction)
    .await?;
    if result.rows_affected() != 1 {
        return Err(Error::NotFound);
    }
    let etag = Uuid::now_v7();
    sqlx::query("UPDATE providers SET etag = $1, updated_at = now() WHERE id = $2")
        .bind(etag)
        .bind(provider_id)
        .execute(&mut *transaction)
        .await?;
    record_success(
        &mut *transaction,
        provenance,
        actor,
        "provider.revoke_credential",
        "provider",
        provider_id,
    )
    .await?;
    complete_idempotency(
        &mut transaction,
        actor,
        "provider.revoke_credential",
        idempotency_key,
        &credential_id.to_string(),
    )
    .await?;
    transaction.commit().await?;
    Ok(etag)
}

async fn rotate_credential_record(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    provider_id: Uuid,
    input: &RotateCredentialInput,
) -> Result<Uuid, Error> {
    let database_version = i32::try_from(input.version)
        .ok()
        .filter(|value| *value > 0)
        .ok_or_else(|| Error::Invalid("credential version is invalid".to_owned()))?;
    let key_version = i32::try_from(input.encrypted.key_version)
        .ok()
        .filter(|value| *value > 0)
        .ok_or_else(|| Error::Invalid("master-key version is invalid".to_owned()))?;
    let provider = lock_provider(transaction, provider_id)
        .await?
        .ok_or(Error::NotFound)?;
    // Under the row lock, so the next version cannot race another writer.
    let next_version: i32 = sqlx::query_scalar::<_, i32>(
        "SELECT COALESCE(max(version), 0) + 1 AS \"next_version\" \
             FROM provider_credential_versions WHERE provider_id = $1",
    )
    .bind(provider_id)
    .fetch_one(&mut **transaction)
    .await?;
    if provider.etag != input.expected_etag {
        return Err(Error::PreconditionFailed);
    }
    if provider.state == "disabled" {
        return Err(Error::InUse);
    }
    if next_version != database_version {
        return Err(Error::PreconditionFailed);
    }
    sqlx::query(
        "INSERT INTO provider_credential_versions \
             (id, provider_id, version, ciphertext, nonce, master_key_version, created_by) \
             VALUES ($1, $2, $3, $4, $5, $6, $7)",
    )
    .bind(input.credential_id)
    .bind(provider_id)
    .bind(database_version)
    .bind(&input.encrypted.ciphertext)
    .bind(input.encrypted.nonce.to_vec())
    .bind(key_version)
    .bind(input.actor)
    .execute(&mut **transaction)
    .await?;
    let etag = Uuid::now_v7();
    // The `FOR UPDATE` read above already refused a disabled provider, and
    // it holds the row for the rest of the transaction. The guard repeats
    // that refusal in the write itself so no future edit can turn rotation
    // into a path that resurrects a disabled provider as a draft, skipping
    // the `restore_as_draft` ceremony.
    let restored = sqlx::query(
        "UPDATE providers SET active_credential_version_id = $1, \
                    state = 'draft'::provider_state, etag = $2, updated_at = now(), \
                    last_probe_at = NULL, last_probe_status = NULL, last_probe_detail = NULL \
             WHERE id = $3 AND state <> 'disabled'::provider_state",
    )
    .bind(input.credential_id)
    .bind(etag)
    .bind(provider_id)
    .execute(&mut **transaction)
    .await?;
    if restored.rows_affected() != 1 {
        return Err(Error::InUse);
    }
    Ok(etag)
}

#[derive(sqlx::FromRow)]
struct ListProviderCredentialsRow {
    id: uuid::Uuid,
    version: i32,
    active: bool,
    draft_selected: bool,
    created_at: chrono::DateTime<chrono::Utc>,
    revoked_at: Option<chrono::DateTime<chrono::Utc>>,
}

#[derive(sqlx::FromRow)]
struct ActiveProviderCredentialSecretRow {
    id: uuid::Uuid,
    version: i32,
    ciphertext: Vec<u8>,
    nonce: Vec<u8>,
    master_key_version: i32,
}

#[derive(sqlx::FromRow)]
struct RevokeProviderCredentialRow {
    etag: uuid::Uuid,
    active_credential_version_id: Option<uuid::Uuid>,
    activated_credential_version_id: Option<uuid::Uuid>,
}
