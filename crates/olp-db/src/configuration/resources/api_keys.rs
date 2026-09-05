use super::*;
use crate::audit_events::record_success;

struct ValidatedApiKeyUpdate<'a> {
    name: &'a str,
    scopes: BTreeSet<&'a str>,
    allowed_routes: BTreeSet<RouteSlug>,
    requests_per_minute: Option<i32>,
    tokens_per_minute: Option<i64>,
    max_concurrency: Option<i32>,
}

fn validated_api_key_update(input: &UpdateApiKeyInput) -> Result<ValidatedApiKeyUpdate<'_>, Error> {
    let name = input.name.trim();
    if name.is_empty() || name.chars().count() > 100 {
        return Err(Error::Invalid(
            "API-key name must contain 1-100 characters".to_owned(),
        ));
    }
    if input.scopes.is_empty() {
        return Err(Error::Invalid(
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
        return Err(Error::Invalid(
            "API-key scopes must be unique inference or models_read values".to_owned(),
        ));
    }
    let allowed_routes = input
        .allowed_routes
        .iter()
        .map(|route| {
            RouteSlug::parse(route.clone())
                .map_err(|error| Error::Invalid(format!("invalid allowlisted route: {error}")))
        })
        .collect::<Result<BTreeSet<_>, _>>()?;
    if allowed_routes.len() != input.allowed_routes.len() {
        return Err(Error::Invalid(
            "allowlisted routes must be unique".to_owned(),
        ));
    }
    if input
        .expires_at
        .is_some_and(|expiration| expiration <= Utc::now())
    {
        return Err(Error::Invalid(
            "API-key expiration must be in the future".to_owned(),
        ));
    }
    let requests_per_minute = input
        .requests_per_minute
        .map(i32::try_from)
        .transpose()
        .map_err(|_| Error::Invalid("RPM limit is too large".to_owned()))?;
    let tokens_per_minute = input
        .tokens_per_minute
        .map(i64::try_from)
        .transpose()
        .map_err(|_| Error::Invalid("TPM limit is too large".to_owned()))?;
    let max_concurrency = input
        .max_concurrency
        .map(i32::try_from)
        .transpose()
        .map_err(|_| Error::Invalid("concurrency limit is too large".to_owned()))?;
    if requests_per_minute == Some(0) || tokens_per_minute == Some(0) || max_concurrency == Some(0)
    {
        return Err(Error::Invalid(
            "hard limits must be positive when configured".to_owned(),
        ));
    }
    if input
        .daily_cost_limit
        .into_iter()
        .chain(input.monthly_cost_limit)
        .any(|value| !crate::valid_cost_limit(value))
    {
        return Err(Error::Invalid(
            "cost limits must have at most 12 integer and 12 fractional digits".to_owned(),
        ));
    }
    Ok(ValidatedApiKeyUpdate {
        name,
        scopes,
        allowed_routes,
        requests_per_minute,
        tokens_per_minute,
        max_concurrency,
    })
}

async fn replace_api_key_associations(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    id: Uuid,
    scopes: BTreeSet<&str>,
    allowed_routes: BTreeSet<RouteSlug>,
) -> Result<(), Error> {
    sqlx::query!("DELETE FROM api_key_scopes WHERE api_key_id = $1", id)
        .execute(&mut **transaction)
        .await?;
    for scope in scopes {
        sqlx::query!(
            "INSERT INTO api_key_scopes (api_key_id, scope) VALUES ($1, $2)",
            id,
            scope
        )
        .execute(&mut **transaction)
        .await?;
    }
    sqlx::query!(
        "DELETE FROM api_key_route_allowlist WHERE api_key_id = $1",
        id
    )
    .execute(&mut **transaction)
    .await?;
    for route in allowed_routes {
        sqlx::query!(
            "INSERT INTO api_key_route_allowlist (api_key_id, route_slug) VALUES ($1, $2)",
            id,
            route.as_str()
        )
        .execute(&mut **transaction)
        .await?;
    }
    Ok(())
}

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
        if ids.is_empty() {
            return Ok(ConfigurationPage {
                items: Vec::new(),
                next_cursor,
            });
        }

        let key_rows = sqlx::query!(
            "SELECT k.id, k.lookup_id, k.name, k.created_by, u.email AS created_by_email, \
                    k.requests_per_minute, k.tokens_per_minute, k.max_concurrency, k.expires_at, \
                    k.daily_cost_limit, k.monthly_cost_limit, k.revoked_at, k.rotated_at, \
                    k.etag, k.created_at \
             FROM api_keys k JOIN users u ON u.id = k.created_by \
             WHERE k.id = ANY($1::uuid[]) \
             ORDER BY k.id",
            &ids
        )
        .fetch_all(self.pool())
        .await?;

        let scope_rows = sqlx::query!(
            "SELECT api_key_id, scope FROM api_key_scopes WHERE api_key_id = ANY($1::uuid[]) ORDER BY api_key_id, scope",
            &ids
        )
        .fetch_all(self.pool())
        .await?;

        let mut scopes_by_id = BTreeMap::<Uuid, Vec<String>>::new();
        for row in scope_rows {
            scopes_by_id
                .entry(row.api_key_id)
                .or_default()
                .push(row.scope);
        }

        let route_rows = sqlx::query!(
            "SELECT api_key_id, route_slug FROM api_key_route_allowlist WHERE api_key_id = ANY($1::uuid[]) ORDER BY api_key_id, route_slug",
            &ids
        )
        .fetch_all(self.pool())
        .await?;

        let mut routes_by_id = BTreeMap::<Uuid, Vec<String>>::new();
        for row in route_rows {
            routes_by_id
                .entry(row.api_key_id)
                .or_default()
                .push(row.route_slug);
        }

        let mut key_map = key_rows
            .into_iter()
            .map(|row| {
                let id = row.id;
                let record = ApiKeyRecord {
                    id,
                    lookup_id: row.lookup_id,
                    name: row.name,
                    created_by: row.created_by,
                    created_by_email: row.created_by_email,
                    scopes: scopes_by_id.remove(&id).unwrap_or_default(),
                    allowed_routes: routes_by_id.remove(&id).unwrap_or_default(),
                    requests_per_minute: row.requests_per_minute,
                    tokens_per_minute: row.tokens_per_minute,
                    max_concurrency: row.max_concurrency,
                    daily_cost_limit: row.daily_cost_limit,
                    monthly_cost_limit: row.monthly_cost_limit,
                    expires_at: row.expires_at,
                    revoked_at: row.revoked_at,
                    rotated_at: row.rotated_at,
                    etag: row.etag,
                    created_at: row.created_at,
                };
                (id, record)
            })
            .collect::<BTreeMap<_, _>>();

        let items = ids
            .into_iter()
            .filter_map(|id| key_map.remove(&id))
            .collect();

        Ok(ConfigurationPage { items, next_cursor })
    }

    pub async fn get_api_key(&self, id: Uuid) -> Result<ApiKeyRecord, Error> {
        let row = sqlx::query!(
            "SELECT k.id, k.lookup_id, k.name, k.created_by, u.email AS created_by_email, \
                    k.requests_per_minute, k.tokens_per_minute, k.max_concurrency, k.expires_at, \
                    k.daily_cost_limit, k.monthly_cost_limit, k.revoked_at, k.rotated_at, \
                    k.etag, k.created_at \
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
            daily_cost_limit: row.daily_cost_limit,
            monthly_cost_limit: row.monthly_cost_limit,
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
        let ValidatedApiKeyUpdate {
            name,
            scopes,
            allowed_routes,
            requests_per_minute,
            tokens_per_minute,
            max_concurrency,
        } = validated_api_key_update(input)?;

        let mut transaction = self
            .pool()
            .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
            .await?;
        prepare_runtime_mutation(&mut transaction).await?;
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
        let etag = Uuid::now_v7();
        let updated = sqlx::query!(
            "UPDATE api_keys SET name = $1, requests_per_minute = $2, tokens_per_minute = $3, \
                    max_concurrency = $4, daily_cost_limit = $5, monthly_cost_limit = $6, \
                    expires_at = $7, etag = $8 \
             WHERE id = $9 AND etag = $10 AND revoked_at IS NULL \
               AND (expires_at IS NULL OR expires_at > now())",
            name,
            requests_per_minute,
            tokens_per_minute,
            max_concurrency,
            input.daily_cost_limit,
            input.monthly_cost_limit,
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
            .ok_or(Error::NotFound)?;
            if row.etag != expected_etag {
                return Err(Error::PreconditionFailed);
            }
            return Err(Error::Invalid(
                "revoked or expired keys cannot be updated".to_owned(),
            ));
        }
        replace_api_key_associations(&mut transaction, id, scopes, allowed_routes).await?;
        record_success(
            &mut *transaction,
            self.provenance(),
            actor,
            "api_key.update",
            "api_key",
            id,
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
            actor,
            idempotency_key,
            daily_cost_limit,
            monthly_cost_limit,
            ..
        } = input;
        if daily_cost_limit
            .flatten()
            .into_iter()
            .chain(monthly_cost_limit.flatten())
            .any(|value| !crate::valid_cost_limit(value))
        {
            return Err(Error::Invalid(
                "cost limits must have at most 12 integer and 12 fractional digits".to_owned(),
            ));
        }
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
        let etag = rotate_api_key_record(&mut transaction, &input).await?;
        record_success(
            &mut *transaction,
            self.provenance(),
            actor,
            "api_key.rotate",
            "api_key",
            id,
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

async fn rotate_api_key_record(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    input: &RotateApiKeyInput<'_>,
) -> Result<Uuid, Error> {
    let RotateApiKeyInput {
        id,
        material,
        expected_etag,
        daily_cost_limit,
        monthly_cost_limit,
        ..
    } = *input;
    let daily_cost_limit_changed = daily_cost_limit.is_some();
    let monthly_cost_limit_changed = monthly_cost_limit.is_some();
    let daily_cost_limit = daily_cost_limit.flatten();
    let monthly_cost_limit = monthly_cost_limit.flatten();
    let etag = Uuid::now_v7();
    let result = sqlx::query!(
        "UPDATE api_keys SET lookup_id = $1, secret_digest = $2, \
                    daily_cost_limit = CASE WHEN $3 THEN $4 ELSE daily_cost_limit END, \
                    monthly_cost_limit = CASE WHEN $5 THEN $6 ELSE monthly_cost_limit END, \
                    etag = $7, rotated_at = now() \
             WHERE id = $8 AND etag = $9 AND revoked_at IS NULL \
               AND (expires_at IS NULL OR expires_at > now())",
        &material.lookup_id,
        material.digest.to_vec(),
        daily_cost_limit_changed,
        daily_cost_limit,
        monthly_cost_limit_changed,
        monthly_cost_limit,
        etag,
        id,
        expected_etag
    )
    .execute(&mut **transaction)
    .await?;
    if result.rows_affected() != 1 {
        let row = sqlx::query!(
            "SELECT etag, revoked_at, expires_at FROM api_keys WHERE id = $1",
            id
        )
        .fetch_optional(&mut **transaction)
        .await?
        .ok_or(Error::NotFound)?;
        if row.etag != expected_etag {
            return Err(Error::PreconditionFailed);
        }
        return Err(Error::Invalid(
            "revoked or expired keys cannot be rotated".to_owned(),
        ));
    }
    Ok(etag)
}
