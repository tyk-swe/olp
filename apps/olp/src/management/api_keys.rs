use std::{
    collections::BTreeSet,
    fmt,
    num::{NonZeroU32, NonZeroU64},
};

use axum::{
    Json,
    extract::{Path, State, rejection::JsonRejection},
    http::{HeaderMap, HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
};
use chrono::{DateTime, Utc};
use olp_domain::{ApiKeyLimits, ApiKeyScope, RouteSlug};
use olp_storage::{
    IdempotencyResponse, NewApiKeyRecord, ReplayableIdempotency, idempotency_fingerprint,
};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;
use uuid::Uuid;

use super::common::*;
use crate::{ApiState, FieldErrors, Problem};

#[derive(Debug, Deserialize, Serialize, ToSchema)]
pub(super) struct CreateApiKeyRequest {
    pub name: String,
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
pub(super) struct CreateApiKeyResponse {
    #[schema(value_type = String, format = Uuid)]
    pub id: Uuid,
    pub lookup_id: String,
    /// Returned only by this creation response.
    #[schema(value_type = String)]
    pub(super) secret: WriteOnlySecret,
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
        (status = 503, description = "Master key, key hasher, or database unavailable", body = Problem)
    )
)]
pub(super) async fn create_api_key(
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<CreateApiKeyRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_key_manager(&principal)?;
    let idempotency_key = require_idempotency_key(&headers)?.to_owned();
    let request = json_payload(payload)?;
    let request_fingerprint = idempotency_fingerprint(&request).map_err(map_persistence)?;
    let mut errors = FieldErrors::new();
    if request.name.trim().is_empty() || request.name.chars().count() > 100 {
        errors.insert(
            "name".to_owned(),
            vec!["Use between 1 and 100 characters.".to_owned()],
        );
    }
    let scopes = request
        .scopes
        .iter()
        .map(|scope| match scope.as_str() {
            "inference" => Ok(ApiKeyScope::Inference),
            "models_read" => Ok(ApiKeyScope::ModelsRead),
            _ => Err(scope.clone()),
        })
        .collect::<Result<Vec<_>, _>>()
        .map_err(|scope| {
            let mut scope_errors = FieldErrors::new();
            scope_errors.insert("scopes".to_owned(), vec![format!("Unknown scope {scope}.")]);
            Problem::validation(scope_errors)
        })?;
    if scopes.is_empty() {
        errors.insert(
            "scopes".to_owned(),
            vec!["Select at least one scope.".to_owned()],
        );
    } else if scopes.iter().copied().collect::<BTreeSet<_>>().len() != scopes.len() {
        errors.insert(
            "scopes".to_owned(),
            vec!["Scope entries must be unique.".to_owned()],
        );
    }
    let allowed_routes = request
        .allowed_routes
        .iter()
        .map(|slug| RouteSlug::parse(slug.clone()))
        .collect::<Result<Vec<_>, _>>()
        .map_err(|error| {
            let mut route_errors = FieldErrors::new();
            route_errors.insert("allowed_routes".to_owned(), vec![error.to_string()]);
            Problem::validation(route_errors)
        })?;
    if allowed_routes
        .iter()
        .cloned()
        .collect::<BTreeSet<_>>()
        .len()
        != allowed_routes.len()
    {
        errors.insert(
            "allowed_routes".to_owned(),
            vec!["Route allowlist entries must be unique.".to_owned()],
        );
    }
    let limits = ApiKeyLimits {
        requests_per_minute: request.requests_per_minute.and_then(NonZeroU32::new),
        tokens_per_minute: request.tokens_per_minute.and_then(NonZeroU64::new),
        concurrency: request.max_concurrency.and_then(NonZeroU32::new),
    };
    if request.requests_per_minute == Some(0) {
        errors.insert(
            "requests_per_minute".to_owned(),
            vec!["Use a positive limit or omit the field.".to_owned()],
        );
    }
    if request.tokens_per_minute == Some(0) {
        errors.insert(
            "tokens_per_minute".to_owned(),
            vec!["Use a positive limit or omit the field.".to_owned()],
        );
    }
    if request.max_concurrency == Some(0) {
        errors.insert(
            "max_concurrency".to_owned(),
            vec!["Use a positive limit or omit the field.".to_owned()],
        );
    }
    if !errors.is_empty() {
        return Err(Problem::validation(errors));
    }
    let master_key = state
        .master_key
        .as_deref()
        .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))?;
    let hasher = state
        .key_hasher
        .as_ref()
        .ok_or_else(|| Problem::service_unavailable("key_hash_key_not_configured"))?;
    let material = hasher.generate_api_key();
    let secret = WriteOnlySecret(material.expose_once().to_owned());
    let record = NewApiKeyRecord {
        name: request.name,
        material,
        scopes,
        allowed_routes,
        limits,
        expires_at: request.expires_at,
        actor: principal.user_id,
        idempotency_key,
    };
    let created = require_store(&state)?
        .create_api_key_record(
            &record,
            ReplayableIdempotency::new(request_fingerprint, master_key),
            move |created| {
                IdempotencyResponse::json(
                    StatusCode::CREATED.as_u16(),
                    &CreateApiKeyResponse {
                        id: created.id,
                        lookup_id: created.lookup_id.clone(),
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
        (status = 200, description = "API key revoked and new runtime published", body = RuntimeGenerationResponse),
        (status = 404, description = "API key not found", body = Problem),
        (status = 412, description = "ETag mismatch", body = Problem)
    )
)]
pub(super) async fn revoke_api_key(
    State(state): State<ApiState>,
    Path(api_key_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_key_manager(&principal)?;
    let idempotency_key = require_idempotency_key(&headers)?;
    let revoked = require_store(&state)?
        .revoke_api_key_record(
            api_key_id,
            if_match(&headers)?,
            principal.user_id,
            idempotency_key,
        )
        .await
        .map_err(map_access)?;
    let mut response = Json(RuntimeGenerationResponse {
        id: revoked.release.generation_id,
        sequence: revoked.release.sequence,
    })
    .into_response();
    response.headers_mut().insert(
        header::ETAG,
        HeaderValue::from_str(&format!("\"{}\"", revoked.etag)).map_err(|_| Problem::internal())?,
    );
    Ok(response)
}
