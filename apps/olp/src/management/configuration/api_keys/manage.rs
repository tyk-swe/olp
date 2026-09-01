use std::fmt;

use axum::{
    Json,
    extract::{Path, Query, State, rejection::JsonRejection},
    http::{HeaderMap, StatusCode},
    response::Response,
};
use chrono::{DateTime, Utc};
use olp_db::{
    configuration::resources::ApiKeyRecord,
    configuration::resources::RotateApiKeyInput,
    idempotency::Replayable,
    idempotency::fingerprint,
    spend::{ApiKeyBudgetStatus, BudgetWindowStatus},
};
use olp_engine::domain::auth::Permission;
use rust_decimal::Decimal;
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;
use uuid::Uuid;

use crate::{
    bootstrap::mode_dependencies::ManagementState,
    management::{
        error_mapping::{map_configuration, map_persistence},
        idempotency::{idempotency_http_response, require_idempotency_key},
        json_payload::{explicit_null, json_payload},
        pagination::{PageQuery, page},
        permissions::require_permission,
        preconditions::{if_match, with_etag},
        principal::{MutationPrincipal, ReadPrincipal},
        response_policy::RuntimeGenerationResponse,
        secrets::WriteOnlySecret,
    },
    public_http::problem::Problem,
};

use super::policy::{
    ApiKeyPolicySnapshot, ExpirationValidation, RawApiKeyPolicy, merge_api_key_policy,
    normalize_api_key_policy, normalize_cost_limit_patch,
};
use crate::management::provenance::Provenance;

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ApiKeyDetailResponse {
    pub id: Uuid,
    pub lookup_id: String,
    pub name: String,
    /// The operator who issued this installation-scoped key.
    pub created_by: Uuid,
    pub created_by_email: String,
    pub scopes: Vec<String>,
    pub allowed_routes: Vec<String>,
    pub requests_per_minute: Option<i32>,
    pub tokens_per_minute: Option<i64>,
    pub max_concurrency: Option<i32>,
    pub budget: ApiKeyBudgetResponse,
    pub expires_at: Option<DateTime<Utc>>,
    pub revoked_at: Option<DateTime<Utc>>,
    pub rotated_at: Option<DateTime<Utc>>,
    pub etag: Uuid,
    pub created_at: DateTime<Utc>,
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ApiKeyBudgetResponse {
    pub daily: ApiKeyBudgetWindowResponse,
    pub monthly: ApiKeyBudgetWindowResponse,
    pub unpriced_attempts: u64,
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ApiKeyBudgetWindowResponse {
    #[schema(required = true, nullable = true)]
    pub limit: Option<String>,
    pub accrued: String,
    pub window_ends_at: DateTime<Utc>,
}

impl ApiKeyBudgetWindowResponse {
    fn new(limit: Option<Decimal>, status: BudgetWindowStatus) -> Self {
        Self {
            limit: limit.map(|value| value.normalize().to_string()),
            accrued: status.accrued.normalize().to_string(),
            window_ends_at: status.window_ends_at,
        }
    }
}

impl ApiKeyDetailResponse {
    fn new(value: ApiKeyRecord, status: ApiKeyBudgetStatus) -> Self {
        Self {
            id: value.id,
            lookup_id: value.lookup_id,
            name: value.name,
            created_by: value.created_by,
            created_by_email: value.created_by_email,
            scopes: value.scopes,
            allowed_routes: value.allowed_routes,
            requests_per_minute: value.requests_per_minute,
            tokens_per_minute: value.tokens_per_minute,
            max_concurrency: value.max_concurrency,
            budget: ApiKeyBudgetResponse {
                daily: ApiKeyBudgetWindowResponse::new(value.daily_cost_limit, status.daily),
                monthly: ApiKeyBudgetWindowResponse::new(value.monthly_cost_limit, status.monthly),
                unpriced_attempts: status.unpriced_attempts,
            },
            expires_at: value.expires_at,
            revoked_at: value.revoked_at,
            rotated_at: value.rotated_at,
            etag: value.etag,
            created_at: value.created_at,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct ApiKeyListResponse {
    pub items: Vec<ApiKeyDetailResponse>,
    pub next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/api-keys",
    tag = "api-keys",
    params(PageQuery),
    responses(
        (status = 200, body = ApiKeyListResponse),
        (status = 400, description = "Malformed query parameters, or an invalid cursor or page size", body = Problem)
    )
)]
pub(crate) async fn list_api_keys(
    State(state): State<ManagementState>,
    Query(query): Query<PageQuery>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<ApiKeyListResponse>, Problem> {
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let page = state
        .store()
        .list_api_keys(cursor, limit)
        .await
        .map_err(map_configuration)?;
    let ids = page.items.iter().map(|key| key.id).collect::<Vec<_>>();
    let mut budgets = state
        .store()
        .api_key_budget_statuses(&ids, Utc::now())
        .await
        .map_err(map_persistence)?;
    let mut items = Vec::with_capacity(page.items.len());
    for key in page.items {
        let budget = budgets.remove(&key.id).ok_or_else(Problem::internal)?;
        items.push(ApiKeyDetailResponse::new(key, budget));
    }
    Ok(Json(ApiKeyListResponse {
        items,
        next_cursor: page.next_cursor.map(|value| value.to_string()),
    }))
}

#[utoipa::path(
    get,
    path = "/api/v1/api-keys/{api_key_id}",
    tag = "api-keys",
    params(("api_key_id" = Uuid, Path)),
    responses((status = 200, body = ApiKeyDetailResponse), (status = 404, body = Problem))
)]
pub(crate) async fn get_api_key(
    State(state): State<ManagementState>,
    Path(api_key_id): Path<Uuid>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ReadConfiguration)?;
    let key = state
        .store()
        .get_api_key(api_key_id)
        .await
        .map_err(map_configuration)?;
    let budget = state
        .store()
        .api_key_budget_status(api_key_id, Utc::now())
        .await
        .map_err(map_persistence)?;
    let key = ApiKeyDetailResponse::new(key, budget);
    let etag = key.etag;
    with_etag(Json(key), etag)
}

/// A merge patch: every field is optional, an omitted field keeps the stored
/// value, and an explicit `null` clears one. Writing absent fields through
/// would silently widen a key's privileges — a rename would drop the route
/// allowlist, the rate limits, and the expiry.
#[derive(Clone, Debug, Default, Deserialize, ToSchema)]
pub(crate) struct UpdateApiKeyRequest {
    #[serde(default)]
    #[schema(nullable = false)]
    pub name: Option<String>,
    #[serde(default)]
    #[schema(nullable = false)]
    pub scopes: Option<Vec<String>>,
    /// Omit to keep the stored allowlist. Send `[]` to clear it; an empty
    /// allowlist places no route restriction on the key.
    #[serde(default)]
    #[schema(nullable = false)]
    pub allowed_routes: Option<Vec<String>>,
    #[serde(default, deserialize_with = "explicit_null")]
    #[schema(value_type = Option<u32>, nullable)]
    pub requests_per_minute: Option<Option<u32>>,
    #[serde(default, deserialize_with = "explicit_null")]
    #[schema(value_type = Option<u64>, nullable)]
    pub tokens_per_minute: Option<Option<u64>>,
    #[serde(default, deserialize_with = "explicit_null")]
    #[schema(value_type = Option<u32>, nullable)]
    pub max_concurrency: Option<Option<u32>>,
    #[serde(default, deserialize_with = "explicit_null")]
    #[schema(value_type = Option<String>, nullable)]
    pub daily_cost_limit: Option<Option<String>>,
    #[serde(default, deserialize_with = "explicit_null")]
    #[schema(value_type = Option<String>, nullable)]
    pub monthly_cost_limit: Option<Option<String>>,
    #[serde(default, deserialize_with = "explicit_null")]
    #[schema(value_type = Option<DateTime<Utc>>, nullable)]
    pub expires_at: Option<Option<DateTime<Utc>>>,
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct ApiKeyMutationResponse {
    pub etag: Uuid,
    pub runtime_generation: RuntimeGenerationResponse,
}

#[utoipa::path(
    patch,
    path = "/api/v1/api-keys/{api_key_id}",
    tag = "api-keys",
    params(
        ("api_key_id" = Uuid, Path),
        ("If-Match" = String, Header, description = "Current API-key ETag")
    ),
    request_body = UpdateApiKeyRequest,
    responses(
        (status = 200, description = "API-key policy updated and runtime published", body = ApiKeyMutationResponse),
        (status = 404, body = Problem),
        (status = 412, body = Problem),
        (status = 422, body = Problem)
    )
)]
pub(crate) async fn update_api_key(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    Path(api_key_id): Path<Uuid>,
    headers: HeaderMap,
    MutationPrincipal(principal): MutationPrincipal,
    payload: Result<Json<UpdateApiKeyRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ManageApiKeys)?;
    let request = json_payload(payload)?;
    let expected_etag = if_match(&headers)?;
    let stored = state
        .store()
        .get_api_key(api_key_id)
        .await
        .map_err(map_configuration)?;
    let (merged, expiration_changed) =
        merge_api_key_policy(ApiKeyPolicySnapshot::from(&stored), &request);
    let expiration_validation = if expiration_changed {
        ExpirationValidation::RequireFuture(Utc::now())
    } else {
        ExpirationValidation::Unchanged
    };
    let input = normalize_api_key_policy(RawApiKeyPolicy::from(&merged), expiration_validation)?
        .into_update_input();
    let result = state
        .store()
        .with_provenance(&provenance)
        .update_api_key(api_key_id, expected_etag, &input, principal.user_id)
        .await
        .map_err(map_configuration)?;
    with_etag(
        Json(ApiKeyMutationResponse {
            etag: result.etag,
            runtime_generation: (&result.release).into(),
        }),
        result.etag,
    )
}

#[derive(Serialize, ToSchema)]
pub(crate) struct RotateApiKeyResponse {
    pub id: Uuid,
    pub lookup_id: String,
    #[schema(value_type = String)]
    secret: WriteOnlySecret,
    pub etag: Uuid,
    pub runtime_generation: RuntimeGenerationResponse,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize, ToSchema)]
pub(crate) struct RotateApiKeyRequest {
    #[serde(default, deserialize_with = "explicit_null")]
    #[serde(skip_serializing_if = "Option::is_none")]
    #[schema(value_type = Option<String>, nullable)]
    pub daily_cost_limit: Option<Option<String>>,
    #[serde(default, deserialize_with = "explicit_null")]
    #[serde(skip_serializing_if = "Option::is_none")]
    #[schema(value_type = Option<String>, nullable)]
    pub monthly_cost_limit: Option<Option<String>>,
}

#[derive(Serialize)]
struct RotateApiKeyFingerprint<'a> {
    api_key_id: Uuid,
    expected_etag: Uuid,
    #[serde(skip_serializing_if = "Option::is_none")]
    daily_cost_limit: Option<Option<&'a str>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    monthly_cost_limit: Option<Option<&'a str>>,
}

impl fmt::Debug for RotateApiKeyResponse {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("RotateApiKeyResponse")
            .field("id", &self.id)
            .field("lookup_id", &self.lookup_id)
            .field("secret", &"[REDACTED]")
            .field("etag", &self.etag)
            .field("runtime_generation", &self.runtime_generation)
            .finish()
    }
}

#[utoipa::path(
    post,
    path = "/api/v1/api-keys/{api_key_id}/rotate",
    tag = "api-keys",
    params(("api_key_id" = Uuid, Path), ("If-Match" = String, Header), ("Idempotency-Key" = String, Header)),
    request_body = Option<RotateApiKeyRequest>,
    responses(
        (status = 200, body = RotateApiKeyResponse),
        (status = 400, description = "Idempotency-Key is missing or invalid", body = Problem),
        (status = 409, description = "Idempotency-Key was already used or is in progress", body = Problem),
        (status = 412, body = Problem),
        (status = 422, description = "Validation failed", body = Problem),
        (status = 503, description = "Master key, authentication HMAC key, or database unavailable", body = Problem)
    )
)]
pub(crate) async fn rotate_api_key(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    Path(api_key_id): Path<Uuid>,
    headers: HeaderMap,
    MutationPrincipal(principal): MutationPrincipal,
    payload: Result<Option<Json<RotateApiKeyRequest>>, JsonRejection>,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ManageApiKeys)?;
    let expected_etag = if_match(&headers)?;
    let idempotency_key = require_idempotency_key(&headers)?.to_owned();
    let request = match payload {
        Ok(Some(Json(request))) => request,
        Ok(None) => RotateApiKeyRequest::default(),
        Err(error) => return Err(json_payload::<RotateApiKeyRequest>(Err(error)).unwrap_err()),
    };
    let (daily_cost_limit, monthly_cost_limit) = normalize_cost_limit_patch(
        request
            .daily_cost_limit
            .as_ref()
            .map(|value| value.as_deref()),
        request
            .monthly_cost_limit
            .as_ref()
            .map(|value| value.as_deref()),
    )?;
    let request_fingerprint = fingerprint(&RotateApiKeyFingerprint {
        api_key_id,
        expected_etag,
        daily_cost_limit: request
            .daily_cost_limit
            .as_ref()
            .map(|value| value.as_deref()),
        monthly_cost_limit: request
            .monthly_cost_limit
            .as_ref()
            .map(|value| value.as_deref()),
    })
    .map_err(map_persistence)?;
    let master_key = state
        .master_key
        .as_deref()
        .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))?;
    let auth_hmac_key = state.auth_hmac_key();
    let material = auth_hmac_key.generate_api_key();
    let secret = WriteOnlySecret::new(material.expose_once().to_owned());
    let result = state
        .store()
        .with_provenance(&provenance)
        .rotate_api_key(
            RotateApiKeyInput {
                id: api_key_id,
                material: &material,
                expected_etag,
                actor: principal.user_id,
                idempotency_key: &idempotency_key,
                daily_cost_limit,
                monthly_cost_limit,
            },
            Replayable::new(request_fingerprint, master_key),
            move |result| {
                olp_db::idempotency::Response::json(
                    StatusCode::OK.as_u16(),
                    &RotateApiKeyResponse {
                        id: result.id,
                        lookup_id: result.lookup_id.clone(),
                        secret,
                        etag: result.etag,
                        runtime_generation: (&result.release).into(),
                    },
                    Some(format!("\"{}\"", result.etag)),
                )
            },
        )
        .await
        .map_err(map_configuration)?;
    idempotency_http_response(result)
}

#[cfg(test)]
mod tests {
    use chrono::TimeZone as _;
    use olp_db::idempotency::fingerprint;
    use serde::Serialize;

    use super::*;

    #[test]
    fn bodyless_rotation_preserves_the_pre_budget_fingerprint() {
        #[derive(Serialize)]
        struct LegacyRotateApiKeyFingerprint {
            api_key_id: Uuid,
            expected_etag: Uuid,
        }

        let api_key_id = Uuid::now_v7();
        let expected_etag = Uuid::now_v7();
        let current = RotateApiKeyFingerprint {
            api_key_id,
            expected_etag,
            daily_cost_limit: None,
            monthly_cost_limit: None,
        };
        let legacy = LegacyRotateApiKeyFingerprint {
            api_key_id,
            expected_etag,
        };
        assert_eq!(
            fingerprint(&current).unwrap(),
            fingerprint(&legacy).unwrap()
        );
    }

    #[test]
    fn budget_windows_serialize_canonical_decimal_strings() {
        let window_ends_at = Utc.with_ymd_and_hms(2026, 10, 6, 0, 0, 0).unwrap();
        let response = ApiKeyBudgetWindowResponse::new(
            Some(Decimal::new(100, 2)),
            BudgetWindowStatus {
                accrued: Decimal::new(2500, 4),
                window_ends_at,
            },
        );
        assert_eq!(response.limit.as_deref(), Some("1"));
        assert_eq!(response.accrued, "0.25");
        assert_eq!(response.window_ends_at, window_ends_at);
    }
}
