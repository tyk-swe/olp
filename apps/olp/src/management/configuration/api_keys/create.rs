use std::fmt;

use axum::{
    Json,
    extract::{Path, State, rejection::JsonRejection},
    http::{HeaderMap, StatusCode},
    response::Response,
};
use chrono::{DateTime, Utc};
use olp_domain::{ApiKeyOwner, ApiKeyOwnerKind, ServiceAccountId, UserId};
use olp_storage::{
    access::NewApiKeyRecord, idempotency::IdempotencyResponse, idempotency::ReplayableIdempotency,
    idempotency::idempotency_fingerprint,
};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;
use uuid::Uuid;

use crate::FieldErrors;
use crate::management::{
    error_mapping::{map_access, map_persistence},
    idempotency::{idempotency_http_response, require_idempotency_key},
    json_payload::json_payload,
    permissions::require_key_manager,
    preconditions::{if_match, with_etag},
    response_policy::RuntimeGenerationResponse,
    secrets::WriteOnlySecret,
    sessions::require_mutation_session,
};
use crate::{ManagementState, Problem};

use super::policy::{ExpirationValidation, RawApiKeyPolicy, normalize_api_key_policy};

#[derive(Debug, Deserialize, Serialize, ToSchema)]
pub(crate) struct CreateApiKeyRequest {
    pub name: String,
    pub owner_kind: Option<ApiKeyOwnerKind>,
    pub owner_id: Option<Uuid>,
    pub team_id: Option<Uuid>,
    pub project_id: Option<Uuid>,
    #[serde(default = "default_key_scopes")]
    pub scopes: Vec<String>,
    #[serde(default)]
    pub allowed_routes: Vec<String>,
    pub requests_per_minute: Option<u32>,
    pub tokens_per_minute: Option<u64>,
    pub max_concurrency: Option<u32>,
    pub expires_at: Option<DateTime<Utc>>,
}

fn default_key_scopes() -> Vec<String> {
    vec!["inference".to_owned()]
}

#[derive(Serialize, ToSchema)]
pub(crate) struct CreateApiKeyResponse {
    #[schema(value_type = String, format = Uuid)]
    pub id: Uuid,
    pub lookup_id: String,
    pub owner_kind: ApiKeyOwnerKind,
    pub owner_id: Uuid,
    pub team_id: Option<Uuid>,
    pub project_id: Option<Uuid>,
    /// Returned only by this creation response.
    #[schema(value_type = String)]
    pub(crate) secret: WriteOnlySecret,
    pub runtime_generation: RuntimeGenerationResponse,
}

#[derive(Serialize, ToSchema)]
pub(crate) struct RevokeApiKeyResponse {
    pub owner_kind: ApiKeyOwnerKind,
    pub owner_id: Uuid,
    pub team_id: Option<Uuid>,
    pub project_id: Option<Uuid>,
    pub runtime_generation: RuntimeGenerationResponse,
}

impl fmt::Debug for CreateApiKeyResponse {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("CreateApiKeyResponse")
            .field("id", &self.id)
            .field("lookup_id", &self.lookup_id)
            .field("secret", &"[REDACTED]")
            .field("runtime_generation", &self.runtime_generation)
            .finish()
    }
}

#[utoipa::path(
    post,
    path = "/api/v1/api-keys",
    tag = "api-keys",
    request_body = CreateApiKeyRequest,
    params(("Idempotency-Key" = String, Header, description = "Unique creation key")),
    responses(
        (status = 201, description = "API key created; secret is shown once", body = CreateApiKeyResponse),
        (status = 403, description = "Insufficient role, CSRF, or origin failure", body = Problem),
        (status = 409, description = "Idempotency conflict or operation in progress", body = Problem),
        (status = 422, description = "Validation failed", body = Problem),
        (status = 503, description = "Master key, authentication HMAC key, or database unavailable", body = Problem)
    )
)]
pub(crate) async fn create_api_key(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    payload: Result<Json<CreateApiKeyRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_key_manager(&principal)?;
    let idempotency_key = require_idempotency_key(&headers)?.to_owned();
    let request = json_payload(payload)?;
    let request_fingerprint = idempotency_fingerprint(&request).map_err(map_persistence)?;
    let policy = normalize_api_key_policy(
        RawApiKeyPolicy::from(&request),
        ExpirationValidation::DeferredToStorage,
    )?;
    let master_key = state
        .master_key
        .as_deref()
        .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))?;
    let auth_hmac_key = state.auth_hmac_key();
    let material = auth_hmac_key.generate_api_key();
    let secret = WriteOnlySecret(material.expose_once().to_owned());
    let (owner, owner_kind, owner_id) = match (request.owner_kind, request.owner_id) {
        (Some(ApiKeyOwnerKind::User), Some(id)) => (
            ApiKeyOwner::User(UserId::from_uuid(id)),
            ApiKeyOwnerKind::User,
            id,
        ),
        (Some(ApiKeyOwnerKind::ServiceAccount), Some(id)) => (
            ApiKeyOwner::ServiceAccount(ServiceAccountId::from_uuid(id)),
            ApiKeyOwnerKind::ServiceAccount,
            id,
        ),
        (None, None) => (
            ApiKeyOwner::User(UserId::from_uuid(principal.user_id)),
            ApiKeyOwnerKind::User,
            principal.user_id,
        ),
        _ => {
            let mut errors = FieldErrors::new();
            errors.insert(
                "owner".to_owned(),
                vec!["owner_kind and owner_id must be supplied together.".to_owned()],
            );
            return Err(Problem::validation(errors));
        }
    };
    let record = NewApiKeyRecord {
        name: policy.name,
        material,
        scopes: policy.scopes,
        allowed_routes: policy.allowed_routes,
        limits: policy.limits,
        expires_at: policy.expires_at,
        owner,
        team_id: request.team_id.map(olp_domain::TeamId::from_uuid),
        project_id: request.project_id.map(olp_domain::ProjectId::from_uuid),
        actor: principal.user_id,
        idempotency_key,
    };
    let created = state
        .store()
        .create_api_key_record(
            &record,
            ReplayableIdempotency::new(request_fingerprint, master_key),
            move |created| {
                IdempotencyResponse::json(
                    StatusCode::CREATED.as_u16(),
                    &CreateApiKeyResponse {
                        id: created.id,
                        lookup_id: created.lookup_id.clone(),
                        owner_kind,
                        owner_id,
                        team_id: request.team_id,
                        project_id: request.project_id,
                        secret,
                        runtime_generation: RuntimeGenerationResponse {
                            id: created.release.generation_id,
                            sequence: created.release.sequence,
                        },
                    },
                    Some(format!("\"{}\"", created.etag)),
                )
            },
        )
        .await
        .map_err(map_access)?;
    idempotency_http_response(created)
}

#[utoipa::path(
    post,
    path = "/api/v1/api-keys/{api_key_id}/revoke",
    tag = "api-keys",
    params(
        ("api_key_id" = Uuid, Path, description = "API key ID"),
        ("If-Match" = String, Header, description = "Current API-key ETag"),
        ("Idempotency-Key" = String, Header, description = "Unique revocation key")
    ),
    responses(
        (status = 200, description = "API key revoked and new runtime published", body = RevokeApiKeyResponse),
        (status = 404, description = "API key not found", body = Problem),
        (status = 412, description = "ETag mismatch", body = Problem)
    )
)]
pub(crate) async fn revoke_api_key(
    State(state): State<ManagementState>,
    Path(api_key_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_key_manager(&principal)?;
    let idempotency_key = require_idempotency_key(&headers)?;
    let revoked = state
        .store()
        .revoke_api_key_record(
            api_key_id,
            if_match(&headers)?,
            principal.user_id,
            idempotency_key,
        )
        .await
        .map_err(map_access)?;
    with_etag(
        Json(RevokeApiKeyResponse {
            owner_kind: revoked.owner_kind,
            owner_id: revoked.owner_id,
            team_id: revoked.team_id,
            project_id: revoked.project_id,
            runtime_generation: RuntimeGenerationResponse {
                id: revoked.release.generation_id,
                sequence: revoked.release.sequence,
            },
        }),
        revoked.etag,
    )
}
