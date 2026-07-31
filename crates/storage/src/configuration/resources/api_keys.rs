use super::{helpers::audit_in_transaction, *};
use olp_domain::{MAX_API_KEY_ALLOWED_ROUTES, MAX_TOKENS_PER_MINUTE};

impl PgStore {
    pub async fn list_api_keys(
        &self,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<ConfigurationPage<ApiKeyRecord>, ConfigurationError> {
        let limit = checked_limit(limit)?;
        let rows = sqlx::query!(
            "SELECT k.id, k.lookup_id, k.name, k.created_by, u.email AS created_by_email, \
                    k.requests_per_minute, k.tokens_per_minute, k.max_concurrency, k.expires_at, \
                    k.revoked_at, k.rotated_at, k.etag, k.created_at, \
                    ARRAY(SELECT scope::text FROM api_key_scopes \
                          WHERE api_key_id = k.id ORDER BY scope) AS \"scopes!\", \
                    ARRAY(SELECT route_slug FROM api_key_route_allowlist \
                          WHERE api_key_id = k.id ORDER BY route_slug) AS \"allowed_routes!\" \
             FROM api_keys k JOIN users u ON u.id = k.created_by \
             WHERE ($1::uuid IS NULL OR k.id > $1) ORDER BY k.id LIMIT $2",
            cursor,
            limit + 1
        )
        .fetch_all(self.pool())
        .await?;
        let (rows, next_cursor) = split_page(rows, limit as usize, |row| row.id);
        let items = rows
            .into_iter()
            .map(|row| ApiKeyRecord {
                id: row.id,
                lookup_id: row.lookup_id,
                name: row.name,
                created_by: row.created_by,
                created_by_email: row.created_by_email,
                scopes: row.scopes,
                allowed_routes: row.allowed_routes,
                requests_per_minute: row.requests_per_minute,
                tokens_per_minute: row.tokens_per_minute,
                max_concurrency: row.max_concurrency,
                expires_at: row.expires_at,
                revoked_at: row.revoked_at,
                rotated_at: row.rotated_at,
                etag: row.etag,
                created_at: row.created_at,
            })
            .collect();
        Ok(ConfigurationPage { items, next_cursor })
    }

    pub async fn get_api_key(&self, id: Uuid) -> Result<ApiKeyRecord, ConfigurationError> {
        let row = sqlx::query!(
            "SELECT k.id, k.lookup_id, k.name, k.created_by, u.email AS created_by_email, \
                    k.requests_per_minute, k.tokens_per_minute, k.max_concurrency, k.expires_at, \
                    k.revoked_at, k.rotated_at, k.etag, k.created_at \
             FROM api_keys k JOIN users u ON u.id = k.created_by WHERE k.id = $1",
            id
        )
        .fetch_optional(self.pool())
        .await?
        .ok_or(ConfigurationError::NotFound)?;
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
    ) -> Result<ApiKeyMutationResult, ConfigurationError> {
        let name = input.name.trim();
        if name.is_empty()
            || name.chars().count() > 100
            || olp_domain::has_unsafe_display_characters(name)
        {
            return Err(ConfigurationError::Invalid(
                "API-key name must contain 1-100 visible characters without control or bidi formatting"
                    .to_owned(),
            ));
        }
        if input.scopes.is_empty() {
            return Err(ConfigurationError::Invalid(
                "at least one API-key scope is required".to_owned(),
            ));
        }
        if input.allowed_routes.len() > MAX_API_KEY_ALLOWED_ROUTES {
            return Err(ConfigurationError::Invalid(format!(
                "API-key route allowlist cannot exceed {MAX_API_KEY_ALLOWED_ROUTES} entries"
            )));
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
            return Err(ConfigurationError::Invalid(
                "API-key scopes must be unique inference or models_read values".to_owned(),
            ));
        }
        let allowed_routes = input
            .allowed_routes
            .iter()
            .map(|route| {
                RouteSlug::parse(route.clone()).map_err(|error| {
                    ConfigurationError::Invalid(format!("invalid allowlisted route: {error}"))
                })
            })
            .collect::<Result<BTreeSet<_>, _>>()?;
        if allowed_routes.len() != input.allowed_routes.len() {
            return Err(ConfigurationError::Invalid(
                "allowlisted routes must be unique".to_owned(),
            ));
        }
        if input
            .expires_at
            .is_some_and(|expiration| expiration <= Utc::now())
        {
            return Err(ConfigurationError::Invalid(
                "API-key expiration must be in the future".to_owned(),
            ));
        }
        let requests_per_minute = input
            .requests_per_minute
            .map(i32::try_from)
            .transpose()
            .map_err(|_| ConfigurationError::Invalid("RPM limit is too large".to_owned()))?;
        if input
            .tokens_per_minute
            .is_some_and(|value| value > MAX_TOKENS_PER_MINUTE)
        {
            return Err(ConfigurationError::Invalid(
                "TPM limit exceeds the distributed limiter maximum".to_owned(),
            ));
        }
        let tokens_per_minute = input
            .tokens_per_minute
            .map(i64::try_from)
            .transpose()
            .map_err(|_| ConfigurationError::Invalid("TPM limit is too large".to_owned()))?;
        let max_concurrency = input
            .max_concurrency
            .map(i32::try_from)
            .transpose()
            .map_err(|_| {
                ConfigurationError::Invalid("concurrency limit is too large".to_owned())
            })?;
        if requests_per_minute == Some(0)
            || tokens_per_minute == Some(0)
            || max_concurrency == Some(0)
        {
            return Err(ConfigurationError::Invalid(
                "hard limits must be positive when configured".to_owned(),
            ));
        }

        let mut transaction = self
            .pool()
            .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
            .await?;
        prepare_runtime_mutation(&mut transaction).await?;
        let allowed_route_names = allowed_routes
            .iter()
            .map(|route| route.as_str().to_owned())
            .collect::<Vec<_>>();
        let active_route_count = sqlx::query_scalar!(
            "SELECT count(*) AS \"value!\" FROM routes WHERE slug = ANY($1::text[])",
            &allowed_route_names
        )
        .fetch_one(&mut *transaction)
        .await?;
        let expected_route_count = i64::try_from(allowed_route_names.len())
            .expect("route allowlists are bounded before database access");
        if active_route_count != expected_route_count {
            return Err(ConfigurationError::Invalid(
                "one or more allowlisted routes are not active".to_owned(),
            ));
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
            input.expires_at,
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
            .ok_or(ConfigurationError::NotFound)?;
            if row.etag != expected_etag {
                return Err(ConfigurationError::PreconditionFailed);
            }
            return Err(ConfigurationError::Invalid(
                "revoked or expired keys cannot be updated".to_owned(),
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
        replay: ReplayableIdempotency<'_>,
        build_response: F,
    ) -> Result<IdempotencyOutcome<ApiKeyRotationResult>, ConfigurationError>
    where
        F: FnOnce(&ApiKeyRotationResult) -> Result<IdempotencyResponse, PersistenceError>,
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
            .ok_or(ConfigurationError::NotFound)?;
            if row.etag != expected_etag {
                return Err(ConfigurationError::PreconditionFailed);
            }
            return Err(ConfigurationError::Invalid(
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
