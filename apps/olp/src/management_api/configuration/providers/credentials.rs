use axum::{
    Json,
    extract::{Path, Query, State, rejection::JsonRejection},
    http::{HeaderMap, StatusCode},
    response::Response,
};
use chrono::{DateTime, Utc};
use olp_providers::ProviderFactory;
use olp_storage::{
    CredentialVersionRecord, ProviderRecord, ReplayableIdempotency, RotateCredentialInput,
    credential_aad, idempotency_fingerprint, idempotency_secret_digest,
};
use serde::{Deserialize, Serialize};
use tracing::error;
use utoipa::ToSchema;
use uuid::Uuid;

use crate::{
    ApiState, Problem,
    management_api::{
        Permission, WriteOnlySecret,
        common::{RuntimeGenerationResponse, idempotency_http_response},
        if_match, require_idempotency_key, require_mutation_session, require_permission,
        require_read_session, require_store,
    },
    provider_adapter::{provider_config, provider_credential},
};

use crate::management_api::configuration::common::{
    PageQuery, json, map_configuration_resource, page, validation, with_etag,
};

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct CredentialResponse {
    pub id: Uuid,
    pub version: i32,
    /// True when this credential is used by the immutable runtime revision.
    pub active: bool,
    /// True when this credential is selected only by the mutable draft.
    pub draft_selected: bool,
    pub created_at: DateTime<Utc>,
    pub revoked_at: Option<DateTime<Utc>>,
}

impl From<CredentialVersionRecord> for CredentialResponse {
    fn from(value: CredentialVersionRecord) -> Self {
        Self {
            id: value.id,
            version: value.version,
            active: value.active,
            draft_selected: value.draft_selected,
            created_at: value.created_at,
            revoked_at: value.revoked_at,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct CredentialListResponse {
    pub items: Vec<CredentialResponse>,
    pub next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/providers/{provider_id}/credentials",
    tag = "providers",
    params(
        ("provider_id" = Uuid, Path),
        ("cursor" = Option<String>, Query),
        ("limit" = Option<u16>, Query, minimum = 1, maximum = 100)
    ),
    responses((status = 200, body = CredentialListResponse), (status = 404, body = Problem))
)]
pub(crate) async fn list_provider_credentials(
    State(state): State<ApiState>,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<CredentialListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let page = require_store(&state)?
        .list_provider_credentials(provider_id, cursor, limit)
        .await
        .map_err(map_configuration_resource)?;
    let items = page.items.into_iter().map(Into::into).collect();
    Ok(Json(CredentialListResponse {
        items,
        next_cursor: page.next_cursor.map(|cursor| cursor.to_string()),
    }))
}

#[derive(Debug, Deserialize, ToSchema)]
pub(crate) struct RotateCredentialRequest {
    #[schema(value_type = String, write_only)]
    credential: WriteOnlySecret,
}

#[derive(Serialize)]
struct RotateProviderCredentialFingerprint {
    provider_id: Uuid,
    expected_etag: Uuid,
    credential_sha256: [u8; 32],
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct ProviderMutationResponse {
    pub provider_id: Uuid,
    pub etag: Uuid,
    pub credential_id: Option<Uuid>,
    pub credential_version: Option<u32>,
    pub runtime_generation: Option<RuntimeGenerationResponse>,
}

#[utoipa::path(
    post,
    path = "/api/v1/providers/{provider_id}/credentials",
    tag = "providers",
    params(
        ("provider_id" = Uuid, Path),
        ("If-Match" = String, Header),
        ("Idempotency-Key" = String, Header)
    ),
    request_body = RotateCredentialRequest,
    responses(
        (status = 201, body = ProviderMutationResponse),
        (status = 409, description = "Idempotency-Key was reused or is in progress", body = Problem),
        (status = 412, body = Problem),
        (status = 503, description = "Master key or database unavailable", body = Problem)
    )
)]
pub(crate) async fn rotate_provider_credential(
    State(state): State<ApiState>,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
    payload: Result<Json<RotateCredentialRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageProviders)?;
    let expected_etag = if_match(&headers)?;
    let idempotency_key = require_idempotency_key(&headers)?.to_owned();
    let request = json(payload)?;
    let request_fingerprint = idempotency_fingerprint(&RotateProviderCredentialFingerprint {
        provider_id,
        expected_etag,
        credential_sha256: idempotency_secret_digest(request.credential.expose().as_bytes()),
    })
    .map_err(crate::management_api::map_persistence)?;
    if request.credential.expose().trim().is_empty() || request.credential.expose().len() > 8_192 {
        return Err(validation(
            "credential",
            "Provide a credential no larger than 8 KiB.",
        ));
    }
    let store = require_store(&state)?;
    let provider = store
        .get_provider(provider_id)
        .await
        .map_err(map_configuration_resource)?;
    validate_rotated_credential(&provider, request.credential.expose())
        .map_err(|detail| validation("credential", &detail))?;
    let version = store
        .next_credential_version_candidate(provider_id)
        .await
        .map_err(map_configuration_resource)?;
    let credential_id = Uuid::now_v7();
    let master_key = state
        .master_key
        .as_deref()
        .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))?;
    let encrypted = master_key
        .seal(
            request.credential.expose().as_bytes(),
            &credential_aad(provider_id, credential_id, version),
        )
        .map_err(|error| {
            error!(%error, "provider credential encryption failed");
            Problem::internal()
        })?;
    let result = store
        .rotate_provider_credential(
            provider_id,
            RotateCredentialInput {
                credential_id,
                version,
                encrypted,
                expected_etag,
                actor: principal.user_id,
                idempotency_key,
            },
            ReplayableIdempotency::new(request_fingerprint, master_key),
            |result| {
                olp_storage::IdempotencyResponse::json(
                    StatusCode::CREATED.as_u16(),
                    &ProviderMutationResponse {
                        provider_id,
                        etag: result.etag,
                        credential_id: Some(credential_id),
                        credential_version: Some(version),
                        runtime_generation: result.release.as_ref().map(|release| {
                            RuntimeGenerationResponse {
                                id: release.generation_id,
                                sequence: release.sequence,
                            }
                        }),
                    },
                    Some(format!("\"{}\"", result.etag)),
                )
            },
        )
        .await
        .map_err(map_configuration_resource)?;
    idempotency_http_response(result)
}

fn validate_rotated_credential(provider: &ProviderRecord, credential: &str) -> Result<(), String> {
    let config = provider_config(provider.into()).map_err(|error| error.to_string())?;
    let credential = provider_credential(&config, Some(credential.as_bytes()))
        .map_err(|error| error.to_string())?;
    ProviderFactory::validate_credential(&config, &credential).map_err(|error| error.to_string())
}

#[utoipa::path(
    post,
    path = "/api/v1/providers/{provider_id}/credentials/{credential_id}/revoke",
    tag = "providers",
    params(
        ("provider_id" = Uuid, Path),
        ("credential_id" = Uuid, Path),
        ("If-Match" = String, Header),
        ("Idempotency-Key" = String, Header)
    ),
    responses((status = 200, body = ProviderMutationResponse), (status = 409, body = Problem), (status = 412, body = Problem))
)]
pub(crate) async fn revoke_provider_credential(
    State(state): State<ApiState>,
    Path((provider_id, credential_id)): Path<(Uuid, Uuid)>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageProviders)?;
    let etag = require_store(&state)?
        .revoke_provider_credential(
            provider_id,
            credential_id,
            if_match(&headers)?,
            principal.user_id,
            require_idempotency_key(&headers)?,
        )
        .await
        .map_err(map_configuration_resource)?;
    with_etag(
        Json(ProviderMutationResponse {
            provider_id,
            etag,
            credential_id: Some(credential_id),
            credential_version: None,
            runtime_generation: None,
        }),
        etag,
    )
}
