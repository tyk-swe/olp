use crate::access::principal::MutationPrincipal;
use std::fmt;

use crate::access::api_keys::lifecycle::NewApiKeyRecord;
use crate::access::policy::Permission;
use crate::database::idempotency::Replayable;
use crate::database::idempotency::Response as IdempotencyResponse;
use crate::database::idempotency::fingerprint;
use crate::database::idempotency::operations;
use axum::Json;
use axum::extract::Path;
use axum::extract::State;
use axum::extract::rejection::JsonRejection;
use axum::http::HeaderMap;
use axum::http::StatusCode;
use axum::response::Response;
use chrono::DateTime;
use chrono::Utc;
use serde::Deserialize;
use serde::Serialize;
use utoipa::ToSchema;
use uuid::Uuid;

use crate::access::permissions::require_permission;
use crate::http::control::error_mapping::map_access;
use crate::http::control::error_mapping::map_persistence;
use crate::http::control::idempotency::MutationReply;
use crate::http::control::idempotency::ReplayableMutation;
use crate::http::control::idempotency::idempotency_http_response;
use crate::http::control::idempotency::require_idempotency_key;
use crate::http::control::json_payload::json_payload;
use crate::http::control::preconditions::if_match;
use crate::http::control::response_policy::RuntimeGenerationResponse;
use crate::http::control::secrets::WriteOnlySecret;
use crate::http::control::state::ManagementState;
use crate::http::problem::Problem;

use crate::access::api_keys::http::policy::ExpirationValidation;
use crate::access::api_keys::http::policy::RawApiKeyPolicy;
use crate::access::api_keys::http::policy::normalize_api_key_policy;
use crate::http::control::provenance::Provenance;

#[derive(Debug, Deserialize, Serialize, ToSchema)]
pub(crate) struct CreateApiKeyRequest {
    pub name: String,
    #[serde(default = "default_key_scopes")]
    pub scopes: Vec<String>,
    #[serde(default)]
    pub allowed_routes: Vec<String>,
    pub requests_per_minute: Option<u32>,
    pub tokens_per_minute: Option<u64>,
    pub max_concurrency: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub daily_cost_limit: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub monthly_cost_limit: Option<String>,
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
    /// Returned only by this creation response.
    #[schema(value_type = String)]
    pub(crate) secret: WriteOnlySecret,
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
    path = "/api/v3/api-keys",
    tag = "api-keys",
    request_body = CreateApiKeyRequest,
    params(("Idempotency-Key" = String, Header, description = "Unique creation key")),
    responses(
        (status = 201, description = "API key created; secret is shown once", body = CreateApiKeyResponse, headers(("Location" = String, description = "Path of the created resource"))),
        (status = 400, description = "Idempotency-Key is missing or invalid", body = Problem, content_type = "application/problem+json"),
        (status = 403, description = "Insufficient role, CSRF, or origin failure", body = Problem, content_type = "application/problem+json"),
        (status = 409, description = "Idempotency conflict or operation in progress", body = Problem, content_type = "application/problem+json"),
        (status = 422, description = "Validation failed", body = Problem, content_type = "application/problem+json"),
        (status = 503, description = "Master key, authentication HMAC key, or database unavailable", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [], "csrfToken" = [])))]
pub(crate) async fn create_api_key(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    headers: HeaderMap,
    MutationPrincipal(principal): MutationPrincipal,
    payload: Result<Json<CreateApiKeyRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ManageApiKeys)?;
    let idempotency_key = require_idempotency_key(&headers)?.to_owned();
    let request = json_payload(payload)?;
    let request_fingerprint = fingerprint(&request).map_err(map_persistence)?;
    let policy = normalize_api_key_policy(
        RawApiKeyPolicy::from(&request),
        ExpirationValidation::DeferredToStorage,
    )?;
    let master_key = state
        .master_key
        .as_deref()
        .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))?;
    let auth_hmac_key = &state.request_boundary.auth_hmac_key;
    let material = auth_hmac_key.generate_api_key();
    let secret = WriteOnlySecret(material.expose_once().to_owned());
    let record = NewApiKeyRecord {
        name: policy.name,
        material,
        scopes: policy.scopes,
        allowed_routes: policy.allowed_routes,
        limits: policy.limits,
        expires_at: policy.expires_at,
        actor: principal.user_id,
        idempotency_key,
    };
    let created = crate::access::api_keys::lifecycle::create_api_key_record(
        &state.request_boundary.pool,
        &provenance,
        &record,
        Replayable::new(request_fingerprint, master_key),
        move |created| {
            IdempotencyResponse::json(
                StatusCode::CREATED.as_u16(),
                &CreateApiKeyResponse {
                    id: created.id,
                    lookup_id: created.lookup_id.clone(),
                    secret,
                    runtime_generation: (&created.release).into(),
                },
                Some(format!("\"{}\"", created.etag)),
            )
            .and_then(|response| response.with_location(format!("/api/v3/api-keys/{}", created.id)))
        },
    )
    .await
    .map_err(map_access)?;
    idempotency_http_response(created)
}

#[utoipa::path(
    post,
    path = "/api/v3/api-keys/{api_key_id}/revoke",
    tag = "api-keys",
    params(
        ("api_key_id" = Uuid, Path, description = "API key ID"),
        ("If-Match" = String, Header, description = "Current API-key ETag"),
        ("Idempotency-Key" = String, Header, description = "Unique revocation key")
    ),
    responses(
        (status = 200, description = "API key revoked and new runtime published; an identical retry replays this response", body = RuntimeGenerationResponse),
        (status = 400, description = "Idempotency-Key is missing or invalid", body = Problem, content_type = "application/problem+json"),
        (status = 404, description = "API key not found", body = Problem, content_type = "application/problem+json"),
        (status = 409, description = "Idempotency-Key was already used or is in progress", body = Problem, content_type = "application/problem+json"),
        (status = 412, description = "ETag mismatch", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [], "csrfToken" = [])))]
pub(crate) async fn revoke_api_key(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    Path(api_key_id): Path<Uuid>,
    headers: HeaderMap,
    MutationPrincipal(principal): MutationPrincipal,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ManageApiKeys)?;
    let expected_etag = if_match(&headers)?;
    let state = &state;
    let provenance = &provenance;
    ReplayableMutation::new(
        state,
        principal.user_id,
        operations::API_KEY_REVOKE,
        &headers,
        &RevokeApiKeyFingerprint {
            api_key_id,
            expected_etag,
        },
    )?
    .run(|key| async move {
        let revoked = crate::access::api_keys::lifecycle::revoke_api_key_record(
            &state.request_boundary.pool,
            provenance,
            api_key_id,
            expected_etag,
            principal.user_id,
            &key,
        )
        .await
        .map_err(map_access)?;
        Ok(MutationReply {
            status: StatusCode::OK,
            body: RuntimeGenerationResponse::from(&revoked.release),
            etag: Some(revoked.etag),
            location: None,
        })
    })
    .await
}

#[derive(Serialize)]
struct RevokeApiKeyFingerprint {
    api_key_id: Uuid,
    expected_etag: Uuid,
}
