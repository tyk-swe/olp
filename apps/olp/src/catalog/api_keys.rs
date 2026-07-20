use std::{collections::BTreeSet, fmt};

use axum::{
    Json,
    extract::{Path, Query, State, rejection::JsonRejection},
    http::{HeaderMap, StatusCode},
    response::Response,
};
use chrono::{DateTime, Utc};
use olp_storage::{
    ApiKeyCatalogRecord, ReplayableIdempotency, RotateApiKeyCatalogInput, UpdateApiKeyCatalogInput,
    idempotency_fingerprint,
};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;
use uuid::Uuid;

use crate::{
    ApiState, FieldErrors, Problem,
    management::{
        Permission, WriteOnlySecret, idempotency_http_response, if_match, require_idempotency_key,
        require_mutation_session, require_permission, require_read_session, require_store,
    },
};

use super::common::{
    PageQuery, RuntimeGenerationCatalogResponse, json, map_catalog, page, with_etag,
};

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(super) struct ApiKeyCatalogResponse {
    pub id: Uuid,
    pub lookup_id: String,
    pub name: String,
    /// The operator who issued this team-scoped key.
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

impl From<ApiKeyCatalogRecord> for ApiKeyCatalogResponse {
    fn from(value: ApiKeyCatalogRecord) -> Self {
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
pub(super) struct ApiKeyListResponse {
    pub items: Vec<ApiKeyCatalogResponse>,
    pub next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/api-keys",
    tag = "api-keys",
    params(("cursor" = Option<String>, Query), ("limit" = Option<u16>, Query)),
    responses((status = 200, body = ApiKeyListResponse))
)]
pub(super) async fn list_api_keys(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<ApiKeyListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let page = require_store(&state)?
        .list_api_key_catalog(cursor, limit)
        .await
        .map_err(map_catalog)?;
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
    responses((status = 200, body = ApiKeyCatalogResponse), (status = 404, body = Problem))
)]
pub(super) async fn get_api_key(
    State(state): State<ApiState>,
    Path(api_key_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let key: ApiKeyCatalogResponse = require_store(&state)?
        .get_api_key_catalog(api_key_id)
        .await
        .map_err(map_catalog)?
        .into();
    let etag = key.etag;
    with_etag(Json(key), etag)
}

#[derive(Clone, Debug, Deserialize, ToSchema)]
pub(super) struct UpdateApiKeyRequest {
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
pub(super) struct ApiKeyMutationResponse {
    pub etag: Uuid,
    pub runtime_generation: RuntimeGenerationCatalogResponse,
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
pub(super) async fn update_api_key(
    State(state): State<ApiState>,
    Path(api_key_id): Path<Uuid>,
    headers: HeaderMap,
    payload: Result<Json<UpdateApiKeyRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageApiKeys)?;
    let request = json(payload)?;
    let mut errors = FieldErrors::new();
    if request.name.trim().is_empty() || request.name.trim().chars().count() > 100 {
        errors.insert(
            "name".to_owned(),
            vec!["Use between 1 and 100 characters.".to_owned()],
        );
    }
    let scopes = request.scopes.iter().collect::<BTreeSet<_>>();
    if scopes.is_empty() {
        errors.insert(
            "scopes".to_owned(),
            vec!["Select at least one scope.".to_owned()],
        );
    } else if scopes.len() != request.scopes.len()
        || !scopes
            .iter()
            .all(|scope| matches!(scope.as_str(), "inference" | "models_read"))
    {
        errors.insert(
            "scopes".to_owned(),
            vec!["Use unique inference or models_read scopes.".to_owned()],
        );
    }
    let mut routes = BTreeSet::new();
    for route in &request.allowed_routes {
        match olp_domain::RouteSlug::parse(route.clone()) {
            Ok(route) => {
                if !routes.insert(route) {
                    errors.insert(
                        "allowed_routes".to_owned(),
                        vec!["Route allowlist entries must be unique.".to_owned()],
                    );
                    break;
                }
            }
            Err(error) => {
                errors.insert("allowed_routes".to_owned(), vec![error.to_string()]);
                break;
            }
        }
    }
    for (field, invalid) in [
        (
            "requests_per_minute",
            request.requests_per_minute == Some(0),
        ),
        ("tokens_per_minute", request.tokens_per_minute == Some(0)),
        ("max_concurrency", request.max_concurrency == Some(0)),
    ] {
        if invalid {
            errors.insert(
                field.to_owned(),
                vec!["Use a positive limit or null.".to_owned()],
            );
        }
    }
    if request
        .expires_at
        .is_some_and(|expiration| expiration <= Utc::now())
    {
        errors.insert(
            "expires_at".to_owned(),
            vec!["Expiration must be in the future or null.".to_owned()],
        );
    }
    if !errors.is_empty() {
        return Err(Problem::validation(errors));
    }
    let result = require_store(&state)?
        .update_api_key_catalog(
            api_key_id,
            if_match(&headers)?,
            &UpdateApiKeyCatalogInput {
                name: request.name,
                scopes: request.scopes,
                allowed_routes: request.allowed_routes,
                requests_per_minute: request.requests_per_minute,
                tokens_per_minute: request.tokens_per_minute,
                max_concurrency: request.max_concurrency,
                expires_at: request.expires_at,
            },
            principal.user_id,
        )
        .await
        .map_err(map_catalog)?;
    with_etag(
        Json(ApiKeyMutationResponse {
            etag: result.etag,
            runtime_generation: RuntimeGenerationCatalogResponse {
                id: result.release.generation_id,
                sequence: result.release.sequence,
            },
        }),
        result.etag,
    )
}

#[derive(Serialize, ToSchema)]
pub(super) struct RotateApiKeyResponse {
    pub id: Uuid,
    pub lookup_id: String,
    #[schema(value_type = String, write_only)]
    secret: WriteOnlySecret,
    pub etag: Uuid,
    pub runtime_generation: RuntimeGenerationCatalogResponse,
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
        (status = 503, description = "Master key, key hasher, or database unavailable", body = Problem)
    )
)]
pub(super) async fn rotate_api_key(
    State(state): State<ApiState>,
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
    .map_err(crate::management::map_persistence)?;
    let master_key = state
        .master_key
        .as_deref()
        .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))?;
    let hasher = state
        .key_hasher
        .as_ref()
        .ok_or_else(|| Problem::service_unavailable("key_hash_key_not_configured"))?;
    let material = hasher.generate_api_key();
    let secret = WriteOnlySecret::new(material.expose_once().to_owned());
    let result = require_store(&state)?
        .rotate_api_key_catalog(
            RotateApiKeyCatalogInput {
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
                        runtime_generation: RuntimeGenerationCatalogResponse {
                            id: result.release.generation_id,
                            sequence: result.release.sequence,
                        },
                    },
                    Some(format!("\"{}\"", result.etag)),
                )
            },
        )
        .await
        .map_err(map_catalog)?;
    idempotency_http_response(result)
}
