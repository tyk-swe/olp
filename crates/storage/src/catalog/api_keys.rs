use super::{helpers::audit_in_transaction, *};

impl PgStore {
    pub async fn list_api_key_catalog(
        &self,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<CatalogPage<ApiKeyCatalogRecord>, CatalogError> {
        let limit = checked_limit(limit)?;
        let rows = sqlx::query(
            "SELECT id FROM api_keys WHERE ($1::uuid IS NULL OR id > $1) ORDER BY id LIMIT $2",
        )
        .bind(cursor)
        .bind(limit + 1)
        .fetch_all(self.pool())
        .await?;
        let (rows, next_cursor) = split_page(rows, limit as usize, |row| row.get::<Uuid, _>("id"));
        let ids: Vec<Uuid> = rows.into_iter().map(|row| row.get("id")).collect();
        let mut items = Vec::with_capacity(ids.len());
        for id in ids {
            items.push(self.get_api_key_catalog(id).await?);
        }
        Ok(CatalogPage { items, next_cursor })
    }

    pub async fn get_api_key_catalog(&self, id: Uuid) -> Result<ApiKeyCatalogRecord, CatalogError> {
        let row = sqlx::query(
            "SELECT k.id, k.lookup_id, k.name, k.created_by, u.email AS created_by_email, \
                    k.requests_per_minute, k.tokens_per_minute, k.max_concurrency, k.expires_at, \
                    k.revoked_at, k.rotated_at, k.etag, k.created_at \
             FROM api_keys k JOIN users u ON u.id = k.created_by WHERE k.id = $1",
        )
        .bind(id)
        .fetch_optional(self.pool())
        .await?
        .ok_or(CatalogError::NotFound)?;
        Ok(ApiKeyCatalogRecord {
            id: row.get("id"),
            lookup_id: row.get("lookup_id"),
            name: row.get("name"),
            created_by: row.get("created_by"),
            created_by_email: row.get("created_by_email"),
            scopes: sqlx::query_scalar("SELECT scope FROM api_key_scopes WHERE api_key_id = $1 ORDER BY scope")
                .bind(id).fetch_all(self.pool()).await?,
            allowed_routes: sqlx::query_scalar("SELECT route_slug FROM api_key_route_allowlist WHERE api_key_id = $1 ORDER BY route_slug")
                .bind(id).fetch_all(self.pool()).await?,
            requests_per_minute: row.get("requests_per_minute"),
            tokens_per_minute: row.get("tokens_per_minute"),
            max_concurrency: row.get("max_concurrency"),
            expires_at: row.get("expires_at"),
            revoked_at: row.get("revoked_at"),
            rotated_at: row.get("rotated_at"),
            etag: row.get("etag"),
            created_at: row.get("created_at"),
        })
    }

    pub async fn update_api_key_catalog(
        &self,
        id: Uuid,
        expected_etag: Uuid,
        input: &UpdateApiKeyCatalogInput,
        actor: Uuid,
    ) -> Result<ApiKeyMutationResult, CatalogError> {
        let name = input.name.trim();
        if name.is_empty() || name.chars().count() > 100 {
            return Err(CatalogError::Invalid(
                "API-key name must contain 1-100 characters".to_owned(),
            ));
        }
        if input.scopes.is_empty() {
            return Err(CatalogError::Invalid(
                "at least one API-key scope is required".to_owned(),
            ));
        }
        let scopes = input
            .scopes
            .iter()
            .map(String::as_str)
            .collect::<BTreeSet<_>>();
        if scopes.len() != input.scopes.len()
            || !scopes
                .iter()
                .all(|scope| matches!(*scope, "inference" | "models_read"))
        {
            return Err(CatalogError::Invalid(
                "API-key scopes must be unique inference or models_read values".to_owned(),
            ));
        }
        let allowed_routes = input
            .allowed_routes
            .iter()
            .map(|route| {
                RouteSlug::parse(route.clone()).map_err(|error| {
                    CatalogError::Invalid(format!("invalid allowlisted route: {error}"))
                })
            })
            .collect::<Result<BTreeSet<_>, _>>()?;
        if allowed_routes.len() != input.allowed_routes.len() {
            return Err(CatalogError::Invalid(
                "allowlisted routes must be unique".to_owned(),
            ));
        }
        if input
            .expires_at
            .is_some_and(|expiration| expiration <= Utc::now())
        {
            return Err(CatalogError::Invalid(
                "API-key expiration must be in the future".to_owned(),
            ));
        }
        let requests_per_minute = input
            .requests_per_minute
            .map(i32::try_from)
            .transpose()
            .map_err(|_| CatalogError::Invalid("RPM limit is too large".to_owned()))?;
        let tokens_per_minute = input
            .tokens_per_minute
            .map(i64::try_from)
            .transpose()
            .map_err(|_| CatalogError::Invalid("TPM limit is too large".to_owned()))?;
        let max_concurrency = input
            .max_concurrency
            .map(i32::try_from)
            .transpose()
            .map_err(|_| CatalogError::Invalid("concurrency limit is too large".to_owned()))?;
        if requests_per_minute == Some(0)
            || tokens_per_minute == Some(0)
            || max_concurrency == Some(0)
        {
            return Err(CatalogError::Invalid(
                "hard limits must be positive when configured".to_owned(),
            ));
        }

        let mut transaction = self
            .pool()
            .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
            .await?;
        prepare_runtime_mutation(&mut transaction).await?;
        for route in &allowed_routes {
            let exists: bool =
                sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM routes WHERE slug = $1)")
                    .bind(route.as_str())
                    .fetch_one(&mut *transaction)
                    .await?;
            if !exists {
                return Err(CatalogError::Invalid(format!(
                    "allowlisted route {route} is not active"
                )));
            }
        }
        let etag = Uuid::now_v7();
        let updated = sqlx::query(
            "UPDATE api_keys SET name = $1, requests_per_minute = $2, tokens_per_minute = $3, \
                    max_concurrency = $4, expires_at = $5, etag = $6 \
             WHERE id = $7 AND etag = $8 AND revoked_at IS NULL \
               AND (expires_at IS NULL OR expires_at > now())",
        )
        .bind(name)
        .bind(requests_per_minute)
        .bind(tokens_per_minute)
        .bind(max_concurrency)
        .bind(input.expires_at)
        .bind(etag)
        .bind(id)
        .bind(expected_etag)
        .execute(&mut *transaction)
        .await?;
        if updated.rows_affected() != 1 {
            let row =
                sqlx::query("SELECT etag, revoked_at, expires_at FROM api_keys WHERE id = $1")
                    .bind(id)
                    .fetch_optional(&mut *transaction)
                    .await?
                    .ok_or(CatalogError::NotFound)?;
            if row.get::<Uuid, _>("etag") != expected_etag {
                return Err(CatalogError::PreconditionFailed);
            }
            return Err(CatalogError::Invalid(
                "revoked or expired keys cannot be updated".to_owned(),
            ));
        }
        sqlx::query("DELETE FROM api_key_scopes WHERE api_key_id = $1")
            .bind(id)
            .execute(&mut *transaction)
            .await?;
        for scope in scopes {
            sqlx::query("INSERT INTO api_key_scopes (api_key_id, scope) VALUES ($1, $2)")
                .bind(id)
                .bind(scope)
                .execute(&mut *transaction)
                .await?;
        }
        sqlx::query("DELETE FROM api_key_route_allowlist WHERE api_key_id = $1")
            .bind(id)
            .execute(&mut *transaction)
            .await?;
        for route in allowed_routes {
            sqlx::query(
                "INSERT INTO api_key_route_allowlist (api_key_id, route_slug) VALUES ($1, $2)",
            )
            .bind(id)
            .bind(route.as_str())
            .execute(&mut *transaction)
            .await?;
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

    pub async fn rotate_api_key_catalog<F>(
        &self,
        input: RotateApiKeyCatalogInput<'_>,
        replay: ReplayableIdempotency<'_>,
        build_response: F,
    ) -> Result<IdempotencyOutcome<ApiKeyRotationResult>, CatalogError>
    where
        F: FnOnce(&ApiKeyRotationResult) -> Result<IdempotencyResponse, PersistenceError>,
    {
        let RotateApiKeyCatalogInput {
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
                return Ok(IdempotencyOutcome::Replayed(response));
            }
            ReplayableIdempotencyClaim::Conflict => {
                transaction.rollback().await?;
                return Err(CatalogError::IdempotencyConflict);
            }
            ReplayableIdempotencyClaim::InProgress => {
                transaction.rollback().await?;
                return Err(CatalogError::IdempotencyInProgress);
            }
        }
        let etag = Uuid::now_v7();
        let result = sqlx::query(
            "UPDATE api_keys SET lookup_id = $1, secret_digest = $2, etag = $3, rotated_at = now() \
             WHERE id = $4 AND etag = $5 AND revoked_at IS NULL \
               AND (expires_at IS NULL OR expires_at > now())",
        )
        .bind(&material.lookup_id)
        .bind(material.digest.to_vec())
        .bind(etag)
        .bind(id)
        .bind(expected_etag)
        .execute(&mut *transaction)
        .await?;
        if result.rows_affected() != 1 {
            let row =
                sqlx::query("SELECT etag, revoked_at, expires_at FROM api_keys WHERE id = $1")
                    .bind(id)
                    .fetch_optional(&mut *transaction)
                    .await?
                    .ok_or(CatalogError::NotFound)?;
            if row.get::<Uuid, _>("etag") != expected_etag {
                return Err(CatalogError::PreconditionFailed);
            }
            return Err(CatalogError::Invalid(
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
        Ok(IdempotencyOutcome::Executed {
            value: result,
            response,
        })
    }
}
