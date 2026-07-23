use super::{helpers::audit_in_transaction, *};

impl PgStore {
    /// Returns the next candidate version without enforcing an HTTP
    /// precondition. The transactional rotation still checks both the ETag and
    /// candidate version after claiming idempotency; this allows an identical
    /// retry with the original ETag to reach its persisted replay response.
    pub async fn next_credential_version_candidate(
        &self,
        provider_id: Uuid,
    ) -> Result<u32, ConfigurationError> {
        let next_version: i32 = sqlx::query_scalar!(
            "SELECT COALESCE(max(cv.version), 0) + 1 AS \"value!\" \
             FROM providers p LEFT JOIN provider_credential_versions cv ON cv.provider_id = p.id \
             WHERE p.id = $1 GROUP BY p.id",
            provider_id
        )
        .fetch_optional(self.pool())
        .await?
        .ok_or(ConfigurationError::NotFound)?;
        u32::try_from(next_version)
            .map_err(|_| ConfigurationError::Invalid("credential version overflow".to_owned()))
    }

    pub async fn rotate_provider_credential<F>(
        &self,
        provider_id: Uuid,
        input: RotateCredentialInput,
        replay: ReplayableIdempotency<'_>,
        build_response: F,
    ) -> Result<IdempotencyOutcome<ProviderMutationResult>, ConfigurationError>
    where
        F: FnOnce(&ProviderMutationResult) -> Result<IdempotencyResponse, PersistenceError>,
    {
        let mut transaction = self
            .pool()
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
                return Ok(IdempotencyOutcome::Replayed(response));
            }
            ReplayableIdempotencyClaim::Conflict => {
                transaction.rollback().await?;
                return Err(ConfigurationError::IdempotencyConflict);
            }
            ReplayableIdempotencyClaim::InProgress => {
                transaction.rollback().await?;
                return Err(ConfigurationError::IdempotencyInProgress);
            }
        }
        let database_version = i32::try_from(input.version)
            .ok()
            .filter(|value| *value > 0)
            .ok_or_else(|| {
                ConfigurationError::Invalid("credential version is invalid".to_owned())
            })?;
        let key_version = i32::try_from(input.encrypted.key_version)
            .ok()
            .filter(|value| *value > 0)
            .ok_or_else(|| {
                ConfigurationError::Invalid("master-key version is invalid".to_owned())
            })?;
        let provider = sqlx::query!(
            "SELECT etag, state::text AS \"state!\", COALESCE((SELECT max(version) FROM \
             provider_credential_versions WHERE provider_id = $1), 0) + 1 AS \"next_version!\" \
             FROM providers WHERE id = $1 FOR UPDATE",
            provider_id
        )
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(ConfigurationError::NotFound)?;
        if provider.etag != input.expected_etag {
            return Err(ConfigurationError::PreconditionFailed);
        }
        if provider.state == "disabled" {
            return Err(ConfigurationError::InUse);
        }
        if provider.next_version != database_version {
            return Err(ConfigurationError::PreconditionFailed);
        }
        sqlx::query!(
            "INSERT INTO provider_credential_versions \
             (id, provider_id, version, ciphertext, nonce, master_key_version, created_by) \
             VALUES ($1, $2, $3, $4, $5, $6, $7)",
            input.credential_id,
            provider_id,
            database_version,
            &input.encrypted.ciphertext,
            input.encrypted.nonce.to_vec(),
            key_version,
            input.actor
        )
        .execute(&mut *transaction)
        .await?;
        let etag = Uuid::now_v7();
        sqlx::query!(
            "UPDATE providers SET active_credential_version_id = $1, \
                    state = 'draft'::provider_state, etag = $2, updated_at = now(), \
                    last_probe_at = NULL, last_probe_status = NULL, last_probe_detail = NULL \
             WHERE id = $3",
            input.credential_id,
            etag,
            provider_id
        )
        .execute(&mut *transaction)
        .await?;
        sqlx::query!(
            "UPDATE model_capabilities SET source = 'declared', certified_at = NULL \
             WHERE provider_model_id IN \
               (SELECT id FROM provider_models WHERE provider_id = $1)",
            provider_id
        )
        .execute(&mut *transaction)
        .await?;
        audit_in_transaction(
            &mut transaction,
            input.actor,
            "provider.rotate_credential",
            "provider",
            provider_id,
            "success",
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
        Ok(IdempotencyOutcome::Executed {
            value: result,
            response,
        })
    }

    pub async fn list_provider_credentials(
        &self,
        provider_id: Uuid,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<ConfigurationPage<CredentialVersionRecord>, ConfigurationError> {
        let limit = checked_limit(limit)?;
        let exists: bool = sqlx::query_scalar!(
            "SELECT EXISTS (SELECT 1 FROM providers WHERE id = $1) AS \"value!\"",
            provider_id
        )
        .fetch_one(self.pool())
        .await?;
        if !exists {
            return Err(ConfigurationError::NotFound);
        }
        let before_version: Option<i32> = match cursor {
            Some(cursor) => Some(
                sqlx::query_scalar!(
                    "SELECT version FROM provider_credential_versions \
                     WHERE provider_id = $1 AND id = $2",
                    provider_id,
                    cursor
                )
                .fetch_optional(self.pool())
                .await?
                .ok_or_else(|| {
                    ConfigurationError::Invalid(
                        "credential pagination cursor is invalid".to_owned(),
                    )
                })?,
            ),
            None => None,
        };
        let items = sqlx::query!(
            "SELECT cv.id, cv.version, \
                    COALESCE(cv.id = ar.credential_version_id, false) AS \"active!\", \
                    COALESCE(p.state = 'draft'::provider_state \
                     AND cv.id = p.active_credential_version_id, false) AS \"draft_selected!\", \
                    cv.created_at, cv.revoked_at FROM provider_credential_versions cv \
             JOIN providers p ON p.id = cv.provider_id \
             LEFT JOIN provider_revisions ar ON ar.id = p.active_revision_id \
             WHERE cv.provider_id = $1 \
             AND ($2::int IS NULL OR cv.version < $2) \
             ORDER BY cv.version DESC LIMIT $3",
            provider_id,
            before_version,
            limit + 1
        )
        .fetch_all(self.pool())
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
        &self,
        provider_id: Uuid,
    ) -> Result<StoredCredentialSecret, ConfigurationError> {
        let row = sqlx::query!(
            "SELECT cv.id, cv.version, cv.ciphertext, cv.nonce, cv.master_key_version \
             FROM providers p JOIN provider_credential_versions cv \
               ON cv.id = p.active_credential_version_id \
             WHERE p.id = $1 AND cv.revoked_at IS NULL",
            provider_id
        )
        .fetch_optional(self.pool())
        .await?
        .ok_or(ConfigurationError::NotFound)?;
        let nonce: Vec<u8> = row.nonce;
        let nonce: [u8; 12] = nonce.try_into().map_err(|_| {
            ConfigurationError::Invalid("stored credential nonce is invalid".to_owned())
        })?;
        let version = u32::try_from(row.version).map_err(|_| {
            ConfigurationError::Invalid("stored credential version is invalid".to_owned())
        })?;
        let key_version = u32::try_from(row.master_key_version).map_err(|_| {
            ConfigurationError::Invalid("stored master-key version is invalid".to_owned())
        })?;
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
        &self,
        provider_id: Uuid,
        credential_id: Uuid,
        expected_etag: Uuid,
        actor: Uuid,
        idempotency_key: &str,
    ) -> Result<Uuid, ConfigurationError> {
        let mut transaction = self.pool().begin().await?;
        if !claim_idempotency(
            &mut transaction,
            actor,
            "provider.revoke_credential",
            idempotency_key,
        )
        .await?
        {
            return Err(ConfigurationError::IdempotencyConflict);
        }
        let provider = sqlx::query!(
            "SELECT p.etag, p.active_credential_version_id, \
                    ar.credential_version_id AS \"activated_credential_version_id?\" \
             FROM providers p LEFT JOIN provider_revisions ar ON ar.id = p.active_revision_id \
             WHERE p.id = $1 FOR UPDATE OF p",
            provider_id
        )
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(ConfigurationError::NotFound)?;
        if provider.etag != expected_etag {
            return Err(ConfigurationError::PreconditionFailed);
        }
        if provider.active_credential_version_id == Some(credential_id)
            || provider.activated_credential_version_id == Some(credential_id)
        {
            return Err(ConfigurationError::InUse);
        }
        // Historic jobs carry their immutable provider revision. Even an
        // otherwise inactive credential remains lifecycle authority until
        // every job that used it has a durable deletion tombstone.
        let used_by_live_media_job: bool = sqlx::query_scalar!(
            "SELECT EXISTS (
               SELECT 1 FROM async_media_jobs j
               JOIN provider_revisions pr ON pr.id = j.provider_revision_id
               WHERE j.provider_id = $1 AND j.lifecycle_state <> 'deleted'
                 AND pr.credential_version_id = $2
             ) AS \"value!\"",
            provider_id,
            credential_id
        )
        .fetch_one(&mut *transaction)
        .await?;
        if used_by_live_media_job {
            return Err(ConfigurationError::InUse);
        }
        let result = sqlx::query!(
            "UPDATE provider_credential_versions SET revoked_at = COALESCE(revoked_at, now()) \
             WHERE id = $1 AND provider_id = $2",
            credential_id,
            provider_id
        )
        .execute(&mut *transaction)
        .await?;
        if result.rows_affected() != 1 {
            return Err(ConfigurationError::NotFound);
        }
        let etag = Uuid::now_v7();
        sqlx::query!(
            "UPDATE providers SET etag = $1, updated_at = now() WHERE id = $2",
            etag,
            provider_id
        )
        .execute(&mut *transaction)
        .await?;
        audit_in_transaction(
            &mut transaction,
            actor,
            "provider.revoke_credential",
            "provider",
            provider_id,
            "success",
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
}
