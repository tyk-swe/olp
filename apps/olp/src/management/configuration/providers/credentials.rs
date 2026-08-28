use axum::{
    Json,
    extract::{Path, Query, State, rejection::JsonRejection},
    http::{HeaderMap, StatusCode},
    response::Response,
};
use chrono::{DateTime, Utc};
use olp_db::{
    configuration::resources::CredentialVersionRecord, configuration::resources::ProviderRecord,
    configuration::resources::RotateCredentialInput, idempotency::Replayable,
    idempotency::fingerprint, idempotency::operations, idempotency::secret_digest,
    security::aad::credential,
};
use olp_engine::{domain::auth::Permission, providers::factory::assembly::Factory};
use serde::{Deserialize, Serialize};
use tracing::error;
use utoipa::ToSchema;
use uuid::Uuid;

use crate::{
    bootstrap::mode_dependencies::ManagementState,
    bootstrap::provider_adapter::{provider_config, provider_credential},
    management::{
        error_mapping::map_configuration,
        idempotency::{
            MutationReply, ReplayableMutation, idempotency_http_response, require_idempotency_key,
        },
        json_payload::json_payload,
        pagination::{PageQuery, page},
        permissions::require_permission,
        preconditions::if_match,
        principal::{MutationPrincipal, ReadPrincipal},
        provenance::Provenance,
        response_policy::RuntimeGenerationResponse,
        secrets::WriteOnlySecret,
    },
    public_http::problem::Problem,
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
        PageQuery,
    ),
    responses(
        (status = 200, body = CredentialListResponse),
        (status = 400, description = "Malformed query parameters, or an invalid cursor or page size", body = Problem),
        (status = 404, body = Problem)
    )
)]
pub(crate) async fn list_provider_credentials(
    State(state): State<ManagementState>,
    Path(provider_id): Path<Uuid>,
    Query(query): Query<PageQuery>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<CredentialListResponse>, Problem> {
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let page = state
        .store()
        .list_provider_credentials(provider_id, cursor, limit)
        .await
        .map_err(map_configuration)?;
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
        (status = 400, description = "Idempotency-Key is missing or invalid", body = Problem),
        (status = 409, description = "Idempotency-Key was reused or is in progress", body = Problem),
        (status = 412, body = Problem),
        (status = 503, description = "Master key or database unavailable", body = Problem)
    )
)]
pub(crate) async fn rotate_provider_credential(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
    MutationPrincipal(principal): MutationPrincipal,
    payload: Result<Json<RotateCredentialRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ManageProviders)?;
    let expected_etag = if_match(&headers)?;
    let idempotency_key = require_idempotency_key(&headers)?.to_owned();
    let request = json_payload(payload)?;
    let request_fingerprint = fingerprint(&RotateProviderCredentialFingerprint {
        provider_id,
        expected_etag,
        credential_sha256: secret_digest(request.credential.expose().as_bytes()),
    })
    .map_err(crate::management::error_mapping::map_persistence)?;
    if request.credential.expose().trim().is_empty() || request.credential.expose().len() > 8_192 {
        return Err(Problem::field_validation(
            "credential",
            "Provide a credential no larger than 8 KiB.",
        ));
    }
    let store = state.store().with_provenance(&provenance);
    let provider = store
        .get_provider(provider_id)
        .await
        .map_err(map_configuration)?;
    validate_rotated_credential(&provider, request.credential.expose())
        .map_err(|detail| Problem::field_validation("credential", detail))?;
    let version = store
        .next_credential_version_candidate(provider_id)
        .await
        .map_err(map_configuration)?;
    let credential_id = Uuid::now_v7();
    let master_key = state
        .master_key
        .as_deref()
        .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))?;
    let encrypted = master_key
        .seal(
            request.credential.expose().as_bytes(),
            &credential(provider_id, credential_id, version),
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
            Replayable::new(request_fingerprint, master_key),
            |result| {
                olp_db::idempotency::Response::json(
                    StatusCode::CREATED.as_u16(),
                    &ProviderMutationResponse {
                        provider_id,
                        etag: result.etag,
                        credential_id: Some(credential_id),
                        credential_version: Some(version),
                        runtime_generation: result.release.as_ref().map(Into::into),
                    },
                    Some(format!("\"{}\"", result.etag)),
                )
            },
        )
        .await
        .map_err(map_configuration)?;
    idempotency_http_response(result)
}

fn validate_rotated_credential(provider: &ProviderRecord, credential: &str) -> Result<(), String> {
    let config = provider_config(provider.into()).map_err(|error| error.to_string())?;
    let credential = provider_credential(&config, Some(credential.as_bytes()))
        .map_err(|error| error.to_string())?;
    Factory::validate_credential(&config, &credential).map_err(|error| error.to_string())
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
    responses(
        (status = 200, body = ProviderMutationResponse),
        (status = 400, description = "Idempotency-Key is missing or invalid", body = Problem),
        (status = 409, description = "Idempotency-Key was already used or is in progress", body = Problem),
        (status = 412, body = Problem)
    )
)]
pub(crate) async fn revoke_provider_credential(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    Path((provider_id, credential_id)): Path<(Uuid, Uuid)>,
    headers: HeaderMap,
    MutationPrincipal(principal): MutationPrincipal,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ManageProviders)?;
    let expected_etag = if_match(&headers)?;
    let state = &state;
    let provenance = &provenance;
    ReplayableMutation::new(
        state,
        principal.user_id,
        operations::PROVIDER_REVOKE_CREDENTIAL,
        &headers,
        &RevokeCredentialFingerprint {
            provider_id,
            credential_id,
            expected_etag,
        },
    )?
    .run(|key| async move {
        let etag = state
            .store()
            .with_provenance(provenance)
            .revoke_provider_credential(
                provider_id,
                credential_id,
                expected_etag,
                principal.user_id,
                &key,
            )
            .await
            .map_err(map_configuration)?;
        Ok(MutationReply {
            status: StatusCode::OK,
            body: ProviderMutationResponse {
                provider_id,
                etag,
                credential_id: Some(credential_id),
                credential_version: None,
                runtime_generation: None,
            },
            etag: Some(etag),
            location: None,
        })
    })
    .await
}

#[derive(Serialize)]
struct RevokeCredentialFingerprint {
    provider_id: Uuid,
    credential_id: Uuid,
    expected_etag: Uuid,
}
