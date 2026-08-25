use std::fmt;

use axum::{
    Json,
    extract::{Path, Query, State, rejection::JsonRejection},
    http::{HeaderMap, StatusCode},
    response::Response,
};
use chrono::{DateTime, Utc};
use olp_db::{
    configuration::resources::ApiKeyRecord, configuration::resources::RotateApiKeyInput,
    idempotency::Replayable, idempotency::fingerprint,
};
use olp_engine::domain::auth::Permission;
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;
use uuid::Uuid;

use crate::{
    bootstrap::mode_dependencies::ManagementState,
    management::{
        error_mapping::map_configuration,
        idempotency::{idempotency_http_response, require_idempotency_key},
        json_payload::{explicit_null, json_payload},
        pagination::{PageQuery, page},
        permissions::require_permission,
        preconditions::{if_match, with_etag},
        response_policy::RuntimeGenerationResponse,
        secrets::WriteOnlySecret,
        sessions::{require_mutation_session, require_read_session},
    },
    public_http::problem::Problem,
};

use super::policy::{
    ApiKeyPolicySnapshot, ExpirationValidation, RawApiKeyPolicy, merge_api_key_policy,
    normalize_api_key_policy,
};

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
    pub expires_at: Option<DateTime<Utc>>,
    pub revoked_at: Option<DateTime<Utc>>,
    pub rotated_at: Option<DateTime<Utc>>,
    pub etag: Uuid,
    pub created_at: DateTime<Utc>,
}

impl From<ApiKeyRecord> for ApiKeyDetailResponse {
    fn from(value: ApiKeyRecord) -> Self {
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
    params(("cursor" = Option<String>, Query), ("limit" = Option<u16>, Query, minimum = 1, maximum = 200, description = "Page size from 1 to 200; defaults to 50")),
    responses((status = 200, body = ApiKeyListResponse))
)]
pub(crate) async fn list_api_keys(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<ApiKeyListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let page = state
        .store()
        .list_api_keys(cursor, limit)
        .await
        .map_err(map_configuration)?;
    Ok(Json(ApiKeyListResponse {
        items: page.items.into_iter().map(Into::into).collect(),
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
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let key: ApiKeyDetailResponse = state
        .store()
        .get_api_key(api_key_id)
        .await
        .map_err(map_configuration)?
        .into();
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
    Path(api_key_id): Path<Uuid>,
    headers: HeaderMap,
    payload: Result<Json<UpdateApiKeyRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
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

#[derive(Serialize)]
struct RotateApiKeyFingerprint {
    api_key_id: Uuid,
    expected_etag: Uuid,
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
    responses(
        (status = 200, body = RotateApiKeyResponse),
        (status = 400, description = "Idempotency-Key is missing or invalid", body = Problem),
        (status = 409, description = "Idempotency-Key was already used or is in progress", body = Problem),
        (status = 412, body = Problem),
        (status = 503, description = "Master key, authentication HMAC key, or database unavailable", body = Problem)
    )
)]
pub(crate) async fn rotate_api_key(
    State(state): State<ManagementState>,
    Path(api_key_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageApiKeys)?;
    let expected_etag = if_match(&headers)?;
    let idempotency_key = require_idempotency_key(&headers)?.to_owned();
    let request_fingerprint = fingerprint(&RotateApiKeyFingerprint {
        api_key_id,
        expected_etag,
    })
    .map_err(crate::management::error_mapping::map_persistence)?;
    let master_key = state
        .master_key
        .as_deref()
        .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))?;
    let auth_hmac_key = state.auth_hmac_key();
    let material = auth_hmac_key.generate_api_key();
    let secret = WriteOnlySecret::new(material.expose_once().to_owned());
    let result = state
        .store()
        .rotate_api_key(
            RotateApiKeyInput {
                id: api_key_id,
                material: &material,
                expected_etag,
                actor: principal.user_id,
                idempotency_key: &idempotency_key,
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
