use std::fmt;

use axum::{
    Json,
    extract::{Path, Query, State, rejection::JsonRejection},
    http::{HeaderMap, StatusCode},
    response::Response,
};
use chrono::{DateTime, Utc};
use olp_storage::{
    ApiKeyRecord, ReplayableIdempotency, RotateApiKeyInput, idempotency_fingerprint,
};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;
use uuid::Uuid;

use crate::{
    ManagementState, Problem,
    management_api::{
        Permission, WriteOnlySecret, common::RuntimeGenerationResponse, idempotency_http_response,
        if_match, require_idempotency_key, require_mutation_session, require_permission,
        require_read_session,
    },
};

use crate::management_api::configuration::common::{
    PageQuery, json, map_configuration_resource, page, with_etag,
};

use super::policy::{ExpirationValidation, RawApiKeyPolicy, normalize_api_key_policy};

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
    params(("cursor" = Option<String>, Query), ("limit" = Option<u16>, Query)),
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
        .map_err(map_configuration_resource)?;
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
        .map_err(map_configuration_resource)?
        .into();
    let etag = key.etag;
    with_etag(Json(key), etag)
}

#[derive(Clone, Debug, Deserialize, ToSchema)]
pub(crate) struct UpdateApiKeyRequest {
    pub name: String,
    pub scopes: Vec<String>,
    #[serde(default)]
    pub allowed_routes: Vec<String>,
    pub requests_per_minute: Option<u32>,
    pub tokens_per_minute: Option<u64>,
    pub max_concurrency: Option<u32>,
    pub expires_at: Option<DateTime<Utc>>,
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
    let request = json(payload)?;
    let input = normalize_api_key_policy(
        RawApiKeyPolicy::from(&request),
        ExpirationValidation::RequireFuture(Utc::now()),
    )?
    .into_update_input();
    let result = state
        .store()
        .update_api_key(api_key_id, if_match(&headers)?, &input, principal.user_id)
        .await
        .map_err(map_configuration_resource)?;
    with_etag(
        Json(ApiKeyMutationResponse {
            etag: result.etag,
            runtime_generation: RuntimeGenerationResponse {
                id: result.release.generation_id,
                sequence: result.release.sequence,
            },
        }),
        result.etag,
    )
}

#[derive(Serialize, ToSchema)]
pub(crate) struct RotateApiKeyResponse {
    pub id: Uuid,
    pub lookup_id: String,
    #[schema(value_type = String, write_only)]
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
        (status = 409, body = Problem),
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
    let request_fingerprint = idempotency_fingerprint(&RotateApiKeyFingerprint {
        api_key_id,
        expected_etag,
    })
    .map_err(crate::management_api::map_persistence)?;
    let master_key = state
        .master_key
        .as_deref()
        .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))?;
    let auth_hmac_key = &state.auth_hmac_key;
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
            ReplayableIdempotency::new(request_fingerprint, master_key),
            move |result| {
                olp_storage::IdempotencyResponse::json(
                    StatusCode::OK.as_u16(),
                    &RotateApiKeyResponse {
                        id: result.id,
                        lookup_id: result.lookup_id.clone(),
                        secret,
                        etag: result.etag,
                        runtime_generation: RuntimeGenerationResponse {
                            id: result.release.generation_id,
                            sequence: result.release.sequence,
                        },
                    },
                    Some(format!("\"{}\"", result.etag)),
                )
            },
        )
        .await
        .map_err(map_configuration_resource)?;
    idempotency_http_response(result)
}
