use super::{helpers::audit_in_transaction, *};

impl Store {
    pub async fn list_api_keys(
        &self,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<ConfigurationPage<ApiKeyRecord>, Error> {
        let limit = checked_limit(limit)?;
        let rows = sqlx::query!(
            "SELECT id FROM api_keys WHERE ($1::uuid IS NULL OR id > $1) ORDER BY id LIMIT $2",
            cursor,
            limit + 1
        )
        .fetch_all(self.pool())
        .await?;
        let (rows, next_cursor) = split_page(rows, limit as usize, |row| row.id);
        let ids: Vec<Uuid> = rows.into_iter().map(|row| row.id).collect();
        let mut items = Vec::with_capacity(ids.len());
        for id in ids {
            items.push(self.get_api_key(id).await?);
        }
        Ok(ConfigurationPage { items, next_cursor })
    }

    pub async fn get_api_key(&self, id: Uuid) -> Result<ApiKeyRecord, Error> {
        let row = sqlx::query!(
            "SELECT k.id, k.lookup_id, k.name, k.created_by, u.email AS created_by_email, \
                    k.requests_per_minute, k.tokens_per_minute, k.max_concurrency, k.expires_at, \
                    k.revoked_at, k.rotated_at, k.etag, k.created_at \
             FROM api_keys k JOIN users u ON u.id = k.created_by WHERE k.id = $1",
            id
        )
        .fetch_optional(self.pool())
        .await?
        .ok_or(Error::NotFound)?;
        Ok(ApiKeyRecord {
            id: row.id,
            lookup_id: row.lookup_id,
            name: row.name,
            created_by: row.created_by,
            created_by_email: row.created_by_email,
            scopes: sqlx::query_scalar!("SELECT scope FROM api_key_scopes WHERE api_key_id = $1 ORDER BY scope", id).fetch_all(self.pool()).await?,
            allowed_routes: sqlx::query_scalar!("SELECT route_slug FROM api_key_route_allowlist WHERE api_key_id = $1 ORDER BY route_slug", id).fetch_all(self.pool()).await?,
            requests_per_minute: row.requests_per_minute,
            tokens_per_minute: row.tokens_per_minute,
            max_concurrency: row.max_concurrency,
            expires_at: row.expires_at,
            revoked_at: row.revoked_at,
            rotated_at: row.rotated_at,
            etag: row.etag,
            created_at: row.created_at,
        })
    }

    pub async fn update_api_key(
        &self,
        id: Uuid,
        expected_etag: Uuid,
        input: &UpdateApiKeyInput,
        actor: Uuid,
    ) -> Result<ApiKeyMutationResult, Error> {
        let mut transaction = self
            .pool()
            .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
            .await?;
        prepare_runtime_mutation(&mut transaction).await?;

        let current = sqlx::query!(
            "SELECT k.id, k.lookup_id, k.name, k.created_by, u.email AS created_by_email, \
                    k.requests_per_minute, k.tokens_per_minute, k.max_concurrency, k.expires_at, \
                    k.revoked_at, k.rotated_at, k.etag, k.created_at \
             FROM api_keys k \
             JOIN users u ON u.id = k.created_by \
             WHERE k.id = $1",
            id
        )
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(Error::NotFound)?;

        if current.etag != expected_etag {
            return Err(Error::PreconditionFailed);
        }
        if current.revoked_at.is_some()
            || current
                .expires_at
                .is_some_and(|expiration| expiration <= Utc::now())
        {
            return Err(Error::Invalid(
                "revoked or expired keys cannot be updated".to_owned(),
            ));
        }

        let name = match &input.name {
            Some(name) => {
                let trimmed = name.trim();
                if trimmed.is_empty() || trimmed.chars().count() > 100 {
                    return Err(Error::Invalid(
                        "API-key name must contain 1-100 characters".to_owned(),
                    ));
                }
                trimmed
            }
            None => &current.name,
        };

        let requests_per_minute = match input.requests_per_minute {
            PatchValue::Preserve => current.requests_per_minute,
            PatchValue::Clear => None,
            PatchValue::Set(limit) => {
                if limit == 0 {
                    return Err(Error::Invalid(
                        "hard limits must be positive when configured".to_owned(),
                    ));
                }
                Some(
                    i32::try_from(limit)
                        .map_err(|_| Error::Invalid("RPM limit is too large".to_owned()))?,
                )
            }
        };

        let tokens_per_minute = match input.tokens_per_minute {
            PatchValue::Preserve => current.tokens_per_minute,
            PatchValue::Clear => None,
            PatchValue::Set(limit) => {
                if limit == 0 {
                    return Err(Error::Invalid(
                        "hard limits must be positive when configured".to_owned(),
                    ));
                }
                Some(
                    i64::try_from(limit)
                        .map_err(|_| Error::Invalid("TPM limit is too large".to_owned()))?,
                )
            }
        };

        let max_concurrency = match input.max_concurrency {
            PatchValue::Preserve => current.max_concurrency,
            PatchValue::Clear => None,
            PatchValue::Set(limit) => {
                if limit == 0 {
                    return Err(Error::Invalid(
                        "hard limits must be positive when configured".to_owned(),
                    ));
                }
                Some(
                    i32::try_from(limit)
                        .map_err(|_| Error::Invalid("concurrency limit is too large".to_owned()))?,
                )
            }
        };

        let expires_at = match input.expires_at {
            PatchValue::Preserve => current.expires_at,
            PatchValue::Clear => None,
            PatchValue::Set(expiration) => {
                if expiration <= Utc::now() {
                    return Err(Error::Invalid(
                        "API-key expiration must be in the future".to_owned(),
                    ));
                }
                Some(expiration)
            }
        };

        if let Some(new_scopes) = &input.scopes {
            if new_scopes.is_empty() {
                return Err(Error::Invalid(
                    "at least one API-key scope is required".to_owned(),
                ));
            }
            let scopes = new_scopes
                .iter()
                .map(String::as_str)
                .collect::<BTreeSet<_>>();
            if scopes.len() != new_scopes.len()
                || !scopes
                    .iter()
                    .all(|scope| matches!(*scope, "inference" | "models_read"))
            {
                return Err(Error::Invalid(
                    "API-key scopes must be unique inference or models_read values".to_owned(),
                ));
            }
            sqlx::query!("DELETE FROM api_key_scopes WHERE api_key_id = $1", id)
                .execute(&mut *transaction)
                .await?;
            for scope in scopes {
                sqlx::query!(
                    "INSERT INTO api_key_scopes (api_key_id, scope) VALUES ($1, $2)",
                    id,
                    scope
                )
                .execute(&mut *transaction)
                .await?;
            }
        }

        if let Some(new_routes) = &input.allowed_routes {
            let allowed_routes = new_routes
                .iter()
                .map(|route| {
                    RouteSlug::parse(route.clone()).map_err(|error| {
                        Error::Invalid(format!("invalid allowlisted route: {error}"))
                    })
                })
                .collect::<Result<BTreeSet<_>, _>>()?;
            if allowed_routes.len() != new_routes.len() {
                return Err(Error::Invalid(
                    "allowlisted routes must be unique".to_owned(),
                ));
            }
            for route in &allowed_routes {
                let exists: bool = sqlx::query_scalar!(
                    "SELECT EXISTS (SELECT 1 FROM routes WHERE slug = $1) AS \"value!\"",
                    route.as_str()
                )
                .fetch_one(&mut *transaction)
                .await?;
                if !exists {
                    return Err(Error::Invalid(format!(
                        "allowlisted route {route} is not active"
                    )));
                }
            }
            sqlx::query!(
                "DELETE FROM api_key_route_allowlist WHERE api_key_id = $1",
                id
            )
            .execute(&mut *transaction)
            .await?;
            for route in allowed_routes {
                sqlx::query!(
                    "INSERT INTO api_key_route_allowlist (api_key_id, route_slug) VALUES ($1, $2)",
                    id,
                    route.as_str()
                )
                .execute(&mut *transaction)
                .await?;
            }
        }

        let etag = Uuid::now_v7();
        let updated = sqlx::query!(
            "UPDATE api_keys SET name = $1, requests_per_minute = $2, tokens_per_minute = $3, \
                    max_concurrency = $4, expires_at = $5, etag = $6 \
             WHERE id = $7 AND etag = $8 AND revoked_at IS NULL \
               AND (expires_at IS NULL OR expires_at > now())",
            name,
            requests_per_minute,
            tokens_per_minute,
            max_concurrency,
            expires_at,
            etag,
            id,
            expected_etag
        )
        .execute(&mut *transaction)
        .await?;
        if updated.rows_affected() != 1 {
            let row = sqlx::query!(
                "SELECT etag, revoked_at, expires_at FROM api_keys WHERE id = $1",
                id
            )
            .fetch_optional(&mut *transaction)
            .await?
            .ok_or(Error::NotFound)?;
            if row.etag != expected_etag {
                return Err(Error::PreconditionFailed);
            }
            return Err(Error::Invalid(
                "revoked or expired keys cannot be updated".to_owned(),
            ));
        }

        audit_in_transaction(
            &mut transaction,
            actor,
            "api_key.update",
            "api_key",
            id,
            "success",
        )
        .await?;
        let release = compile_and_publish_runtime_in_transaction(&mut transaction, actor).await?;
        transaction.commit().await?;
        Ok(ApiKeyMutationResult { etag, release })
    }

    pub async fn rotate_api_key<F>(
        &self,
        input: RotateApiKeyInput<'_>,
        replay: Replayable<'_>,
        build_response: F,
    ) -> Result<Outcome<ApiKeyRotationResult>, Error>
    where
        F: FnOnce(&ApiKeyRotationResult) -> Result<Response, PersistenceError>,
    {
        let RotateApiKeyInput {
            id,
            material,
            expected_etag,
            actor,
            idempotency_key,
        } = input;
        let mut transaction = self
            .pool()
            .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
            .await?;
        match claim_replayable_idempotency(
            &mut transaction,
            actor,
            "api_key.rotate",
            idempotency_key,
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
        let etag = Uuid::now_v7();
        let result = sqlx::query!(
            "UPDATE api_keys SET lookup_id = $1, secret_digest = $2, etag = $3, rotated_at = now() \
             WHERE id = $4 AND etag = $5 AND revoked_at IS NULL \
               AND (expires_at IS NULL OR expires_at > now())",
            &material.lookup_id,
            material.digest.to_vec(),
            etag,
            id,
            expected_etag
        )
        .execute(&mut *transaction)
        .await?;
        if result.rows_affected() != 1 {
            let row = sqlx::query!(
                "SELECT etag, revoked_at, expires_at FROM api_keys WHERE id = $1",
                id
            )
            .fetch_optional(&mut *transaction)
            .await?
            .ok_or(Error::NotFound)?;
            if row.etag != expected_etag {
                return Err(Error::PreconditionFailed);
            }
            return Err(Error::Invalid(
                "revoked or expired keys cannot be rotated".to_owned(),
            ));
        }
        audit_in_transaction(
            &mut transaction,
            actor,
            "api_key.rotate",
            "api_key",
            id,
            "success",
        )
        .await?;
        let release = compile_and_publish_runtime_in_transaction(&mut transaction, actor).await?;
        let result = ApiKeyRotationResult {
            id,
            lookup_id: material.lookup_id.clone(),
            etag,
            release,
        };
        let response = build_response(&result)?;
        complete_replayable_idempotency(
            &mut transaction,
            actor,
            "api_key.rotate",
            idempotency_key,
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
}
