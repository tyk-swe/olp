use axum::{
    Json,
    extract::{Path, Query, State, rejection::JsonRejection},
    http::{HeaderMap, StatusCode},
    response::Response,
};
use chrono::{DateTime, Utc};
use olp_domain::ProviderKind;
use olp_providers::{
    CredentialKind, ProviderConfig, ProviderCredential, ProviderError, ProviderFacade,
    ProviderFactory,
};
use olp_storage::{
    CredentialVersionRecord, ProviderCatalogRecord, ReplayableIdempotency, RotateCredentialInput,
    UpdateProviderCatalog, credential_aad, idempotency_fingerprint, idempotency_secret_digest,
};
use serde::{Deserialize, Serialize};
use tracing::error;
use utoipa::ToSchema;
use uuid::Uuid;
use zeroize::Zeroizing;

use crate::{
    ApiState, Problem,
    management::{
        Permission, WriteOnlySecret, idempotency_http_response, if_match, require_idempotency_key,
        require_mutation_session, require_permission, require_read_session, require_store,
    },
};

use super::common::{
    PageQuery, RuntimeGenerationCatalogResponse, json, map_catalog, page, validation, with_etag,
};

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(super) struct ProviderSummaryResponse {
    pub id: Uuid,
    pub name: String,
    pub kind: String,
    pub state: String,
    pub connector_ready: bool,
    pub etag: Uuid,
    pub active_revision: Option<u32>,
    pub pending_activation: bool,
    pub last_probe_at: Option<DateTime<Utc>>,
    pub last_probe_status: Option<String>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    pub model_count: u64,
    pub enabled_model_count: u64,
    pub capability_count: u64,
    pub certified_capability_count: u64,
}

impl From<ProviderCatalogRecord> for ProviderSummaryResponse {
    fn from(value: ProviderCatalogRecord) -> Self {
        Self {
            id: value.id,
            name: value.name,
            kind: value.kind.to_string(),
            state: value.state.to_string(),
            connector_ready: value.connector_ready,
            etag: value.etag,
            active_revision: value.active_revision,
            pending_activation: value.pending_activation,
            last_probe_at: value.last_probe_at,
            last_probe_status: value.last_probe_status,
            created_at: value.created_at,
            updated_at: value.updated_at,
            model_count: value.model_count,
            enabled_model_count: value.enabled_model_count,
            capability_count: value.capability_count,
            certified_capability_count: value.certified_capability_count,
        }
    }
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(super) struct ProviderCatalogResponse {
    pub id: Uuid,
    pub name: String,
    pub kind: String,
    pub state: String,
    pub endpoint: Option<String>,
    pub cloud_region: Option<String>,
    pub cloud_project: Option<String>,
    pub deployment: Option<String>,
    pub api_version: Option<String>,
    pub auth_mode: String,
    pub connector_ready: bool,
    pub etag: Uuid,
    pub active_revision: Option<u32>,
    pub pending_activation: bool,
    pub draft_credential_id: Option<Uuid>,
    pub draft_credential_version: Option<i32>,
    pub runtime_credential_id: Option<Uuid>,
    pub runtime_credential_version: Option<i32>,
    pub last_probe_at: Option<DateTime<Utc>>,
    pub last_probe_status: Option<String>,
    pub last_probe_detail: Option<String>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    pub model_count: u64,
    pub enabled_model_count: u64,
    pub capability_count: u64,
    pub certified_capability_count: u64,
}

impl From<ProviderCatalogRecord> for ProviderCatalogResponse {
    fn from(value: ProviderCatalogRecord) -> Self {
        Self {
            id: value.id,
            name: value.name,
            kind: value.kind.to_string(),
            state: value.state.to_string(),
            endpoint: value.endpoint,
            cloud_region: value.cloud_region,
            cloud_project: value.cloud_project,
            deployment: value.deployment,
            api_version: value.api_version,
            auth_mode: value.auth_mode.to_string(),
            connector_ready: value.connector_ready,
            etag: value.etag,
            active_revision: value.active_revision,
            pending_activation: value.pending_activation,
            draft_credential_id: value.draft_credential_id,
            draft_credential_version: value.draft_credential_version,
            runtime_credential_id: value.runtime_credential_id,
            runtime_credential_version: value.runtime_credential_version,
            last_probe_at: value.last_probe_at,
            last_probe_status: value.last_probe_status,
            last_probe_detail: value.last_probe_detail,
            created_at: value.created_at,
            updated_at: value.updated_at,
            model_count: value.model_count,
            enabled_model_count: value.enabled_model_count,
            capability_count: value.capability_count,
            certified_capability_count: value.certified_capability_count,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct ProviderListResponse {
    pub items: Vec<ProviderSummaryResponse>,
    pub next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/providers",
    tag = "providers",
    params(
        ("cursor" = Option<String>, Query),
        ("limit" = Option<u16>, Query, minimum = 1, maximum = 100)
    ),
    responses(
        (status = 200, body = ProviderListResponse),
        (status = 401, body = Problem),
        (status = 403, body = Problem)
    )
)]
pub(super) async fn list_providers(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<ProviderListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let page = require_store(&state)?
        .list_provider_catalog(cursor, limit)
        .await
        .map_err(map_catalog)?;
    Ok(Json(ProviderListResponse {
        items: page.items.into_iter().map(Into::into).collect(),
        next_cursor: page.next_cursor.map(|value| value.to_string()),
    }))
}

#[utoipa::path(
    get,
    path = "/api/v1/providers/{provider_id}",
    tag = "providers",
    params(("provider_id" = Uuid, Path)),
    responses((status = 200, body = ProviderCatalogResponse), (status = 404, body = Problem))
)]
pub(super) async fn get_provider(
    State(state): State<ApiState>,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let provider: ProviderCatalogResponse = require_store(&state)?
        .get_provider_catalog(provider_id)
        .await
        .map_err(map_catalog)?
        .into();
    let etag = provider.etag;
    with_etag(Json(provider), etag)
}

#[derive(Debug, Deserialize, ToSchema)]
pub(super) struct UpdateProviderRequest {
    pub name: String,
    pub endpoint: Option<String>,
    pub cloud_region: Option<String>,
    pub cloud_project: Option<String>,
    pub deployment: Option<String>,
    pub api_version: Option<String>,
    pub auth_mode: String,
}

#[utoipa::path(
    patch,
    path = "/api/v1/providers/{provider_id}",
    tag = "providers",
    params(("provider_id" = Uuid, Path), ("If-Match" = String, Header)),
    request_body = UpdateProviderRequest,
    responses((status = 200, body = ProviderCatalogResponse), (status = 412, body = Problem), (status = 422, body = Problem))
)]
pub(super) async fn update_provider(
    State(state): State<ApiState>,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
    payload: Result<Json<UpdateProviderRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageProviders)?;
    let request = json(payload)?;
    let store = require_store(&state)?;
    let current = store
        .get_provider_catalog(provider_id)
        .await
        .map_err(map_catalog)?;
    validate_provider_update(&current, &request)
        .map_err(|detail| validation("provider", &detail))?;
    let etag = store
        .update_provider_catalog(
            provider_id,
            if_match(&headers)?,
            &UpdateProviderCatalog {
                name: request.name,
                endpoint: request.endpoint,
                cloud_region: request.cloud_region,
                cloud_project: request.cloud_project,
                deployment: request.deployment,
                api_version: request.api_version,
                auth_mode: request.auth_mode.parse().map_err(|_| {
                    validation("auth_mode", "Provider authentication mode is invalid.")
                })?,
            },
            principal.user_id,
        )
        .await
        .map_err(map_catalog)?;
    let provider: ProviderCatalogResponse = store
        .get_provider_catalog(provider_id)
        .await
        .map_err(map_catalog)?
        .into();
    with_etag(Json(provider), etag)
}

#[utoipa::path(
    post,
    path = "/api/v1/providers/{provider_id}/disable",
    tag = "providers",
    params(
        ("provider_id" = Uuid, Path),
        ("If-Match" = String, Header),
        ("Idempotency-Key" = String, Header)
    ),
    responses(
        (status = 200, body = ProviderMutationResponse),
        (status = 409, description = "Provider is still referenced by an active route", body = Problem),
        (status = 412, body = Problem)
    )
)]
pub(super) async fn disable_provider(
    State(state): State<ApiState>,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageProviders)?;
    let result = require_store(&state)?
        .disable_provider_catalog(
            provider_id,
            if_match(&headers)?,
            principal.user_id,
            require_idempotency_key(&headers)?,
        )
        .await
        .map_err(map_catalog)?;
    with_etag(
        Json(ProviderMutationResponse {
            provider_id,
            etag: result.etag,
            credential_id: None,
            credential_version: None,
            runtime_generation: result
                .release
                .map(|release| RuntimeGenerationCatalogResponse {
                    id: release.generation_id,
                    sequence: release.sequence,
                }),
        }),
        result.etag,
    )
}

#[utoipa::path(
    post,
    path = "/api/v1/providers/{provider_id}/restore-as-draft",
    tag = "providers",
    params(
        ("provider_id" = Uuid, Path),
        ("If-Match" = String, Header),
        ("Idempotency-Key" = String, Header)
    ),
    responses((status = 200, body = ProviderCatalogResponse), (status = 412, body = Problem))
)]
pub(super) async fn restore_provider_as_draft(
    State(state): State<ApiState>,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageProviders)?;
    let store = require_store(&state)?;
    let etag = store
        .restore_provider_as_draft_catalog(
            provider_id,
            if_match(&headers)?,
            principal.user_id,
            require_idempotency_key(&headers)?,
        )
        .await
        .map_err(map_catalog)?;
    let provider: ProviderCatalogResponse = store
        .get_provider_catalog(provider_id)
        .await
        .map_err(map_catalog)?
        .into();
    with_etag(Json(provider), etag)
}

fn validate_provider_update(
    provider: &ProviderCatalogRecord,
    request: &UpdateProviderRequest,
) -> Result<(), String> {
    if request.auth_mode != provider.auth_mode.as_str() {
        return Err(
            "Provider authentication mode is immutable; create a separate provider to change identity mode."
                .to_owned(),
        );
    }
    let model = provider
        .probe_model
        .clone()
        .unwrap_or_else(|| "configuration-probe".to_owned());
    let kind = provider.kind;
    match kind {
        ProviderKind::OpenAi => {
            require_auth_mode(request, "api_key")?;
            reject_cloud_fields(request)?;
            if request.endpoint.is_some() {
                return Err("Native OpenAI uses the official endpoint; use an OpenAI-compatible provider for a custom endpoint.".to_owned());
            }
        }
        ProviderKind::OpenAiCompatible => {
            require_auth_mode(request, "api_key")?;
            reject_cloud_fields(request)?;
            if request.endpoint.is_none() {
                return Err("An OpenAI-compatible HTTPS endpoint is required".to_owned());
            }
        }
        ProviderKind::Anthropic => {
            require_auth_mode(request, "api_key")?;
            reject_cloud_fields(request)?;
            if request.endpoint.is_some() {
                return Err("Native Anthropic uses the official endpoint.".to_owned());
            }
        }
        ProviderKind::Gemini => {
            require_auth_mode(request, "api_key")?;
            reject_cloud_fields(request)?;
            if request.endpoint.is_some() {
                return Err("Gemini Developer API uses the official endpoint.".to_owned());
            }
        }
        ProviderKind::VertexAi => {
            if !matches!(request.auth_mode.as_str(), "adc" | "service_account") {
                return Err("Vertex AI authentication must be adc or service_account.".to_owned());
            }
            if request.endpoint.is_some()
                || request.deployment.is_some()
                || request.api_version.is_some()
            {
                return Err(
                    "Vertex AI endpoint is derived from its project and location and does not use deployment/API-version fields.".to_owned(),
                );
            }
            if request.cloud_project.is_none() {
                return Err("Vertex AI cloud project is required".to_owned());
            }
            if request.cloud_region.is_none() {
                return Err("Vertex AI cloud location is required".to_owned());
            }
        }
        ProviderKind::Bedrock => {
            if !matches!(request.auth_mode.as_str(), "default_chain" | "static") {
                return Err("Bedrock authentication must be default_chain or static.".to_owned());
            }
            if request.endpoint.is_some()
                || request.cloud_project.is_some()
                || request.deployment.is_some()
                || request.api_version.is_some()
            {
                return Err("Bedrock accepts only an AWS region and credential mode.".to_owned());
            }
            if request.cloud_region.is_none() {
                return Err("Bedrock AWS region is required".to_owned());
            }
        }
        ProviderKind::AzureOpenAi => {
            require_auth_mode(request, "api_key")?;
            if request.cloud_region.is_some() || request.cloud_project.is_some() {
                return Err("Azure OpenAI does not accept project or region fields.".to_owned());
            }
            if request.endpoint.is_none() {
                return Err("Azure OpenAI resource endpoint is required".to_owned());
            }
            if request.deployment.is_none() {
                return Err("Azure OpenAI deployment is required".to_owned());
            }
            if request.api_version.is_none() {
                return Err("Azure OpenAI API version is required".to_owned());
            }
        }
    }
    let config = provider_config_fields(
        kind,
        request.endpoint.clone(),
        request.cloud_region.clone(),
        request.cloud_project.clone(),
        request.deployment.clone(),
        request.api_version.clone(),
        request
            .auth_mode
            .parse()
            .map_err(|_| "Provider authentication mode is invalid".to_owned())?,
        Some(model),
    )?;
    ProviderFactory::validate(&config).map_err(|error| error.to_string())
}

fn require_auth_mode(request: &UpdateProviderRequest, expected: &str) -> Result<(), String> {
    if request.auth_mode == expected {
        Ok(())
    } else {
        Err(format!("Provider authentication must be {expected}."))
    }
}

fn reject_cloud_fields(request: &UpdateProviderRequest) -> Result<(), String> {
    if request.cloud_region.is_some()
        || request.cloud_project.is_some()
        || request.deployment.is_some()
        || request.api_version.is_some()
    {
        Err("This connector does not accept cloud project, region, deployment, or API-version fields.".to_owned())
    } else {
        Ok(())
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct CredentialResponse {
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
pub(super) struct CredentialListResponse {
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
pub(super) async fn list_provider_credentials(
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
        .map_err(map_catalog)?;
    let items = page.items.into_iter().map(Into::into).collect();
    Ok(Json(CredentialListResponse {
        items,
        next_cursor: page.next_cursor.map(|cursor| cursor.to_string()),
    }))
}

#[derive(Debug, Deserialize, ToSchema)]
pub(super) struct RotateCredentialRequest {
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
pub(super) struct ProviderMutationResponse {
    pub provider_id: Uuid,
    pub etag: Uuid,
    pub credential_id: Option<Uuid>,
    pub credential_version: Option<u32>,
    pub runtime_generation: Option<RuntimeGenerationCatalogResponse>,
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
pub(super) async fn rotate_provider_credential(
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
    .map_err(crate::management::map_persistence)?;
    if request.credential.expose().trim().is_empty() || request.credential.expose().len() > 8_192 {
        return Err(validation(
            "credential",
            "Provide a credential no larger than 8 KiB.",
        ));
    }
    let store = require_store(&state)?;
    let provider = store
        .get_provider_catalog(provider_id)
        .await
        .map_err(map_catalog)?;
    validate_rotated_credential(&provider, request.credential.expose())
        .map_err(|detail| validation("credential", &detail))?;
    let version = store
        .next_credential_version_candidate(provider_id)
        .await
        .map_err(map_catalog)?;
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
                            RuntimeGenerationCatalogResponse {
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
        .map_err(map_catalog)?;
    idempotency_http_response(result)
}

fn validate_rotated_credential(
    provider: &ProviderCatalogRecord,
    credential: &str,
) -> Result<(), String> {
    let config = provider_config(provider)?;
    let credential = catalog_credential(&config, Some(credential.as_bytes()))
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
pub(super) async fn revoke_provider_credential(
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
        .map_err(map_catalog)?;
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

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct ProbeResponse {
    pub provider_id: Uuid,
    pub succeeded: bool,
    pub checked_at: DateTime<Utc>,
    pub probe_type: String,
    pub detail: String,
    pub discovered_models: Option<usize>,
}

#[utoipa::path(
    post,
    path = "/api/v1/providers/{provider_id}/probe",
    tag = "providers",
    params(
        ("provider_id" = Uuid, Path),
        ("If-Match" = String, Header, description = "Exact provider draft ETag being probed")
    ),
    responses(
        (status = 200, body = ProbeResponse),
        (status = 412, description = "Provider changed before probe evidence could be recorded", body = Problem),
        (status = 422, body = Problem)
    )
)]
pub(super) async fn probe_provider(
    State(state): State<ApiState>,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageProviders)?;
    let store = require_store(&state)?;
    let expected_etag = if_match(&headers)?;
    let provider = store
        .get_provider_catalog(provider_id)
        .await
        .map_err(map_catalog)?;
    if provider.etag != expected_etag {
        return Err(map_catalog(olp_storage::CatalogError::PreconditionFailed));
    }
    let connector = catalog_connector(&state, provider_id).await?;
    // Configuration-only checks are intentionally not accepted as activation
    // evidence. A probe always performs a bounded credentialed upstream call,
    // and persistence binds the result to the exact ETag captured above.
    let probe = connector.discover_models().await;
    let (succeeded, detail, discovered_models) = match probe {
        Ok(models) => (
            true,
            "Credentialed connector request succeeded.".to_owned(),
            Some(models.len()),
        ),
        Err(detail) => (false, detail, None),
    };
    let checked_at = store
        .record_provider_probe(
            provider_id,
            expected_etag,
            succeeded,
            &detail,
            principal.user_id,
        )
        .await
        .map_err(map_catalog)?;
    if !succeeded {
        return Err(validation("provider", &detail));
    }
    with_etag(
        Json(ProbeResponse {
            provider_id,
            succeeded,
            checked_at,
            probe_type: if provider.kind == ProviderKind::AzureOpenAi {
                "deployment_capability".to_owned()
            } else {
                "connector_connectivity".to_owned()
            },
            detail,
            discovered_models,
        }),
        expected_etag,
    )
}

pub(super) async fn catalog_connector(
    state: &ApiState,
    provider_id: Uuid,
) -> Result<ProviderFacade, Problem> {
    let store = require_store(state)?;
    let provider = store
        .get_provider_catalog(provider_id)
        .await
        .map_err(map_catalog)?;
    let config = provider_config(&provider).map_err(|detail| validation("provider", &detail))?;
    #[cfg(any(test, feature = "test-util"))]
    if let Some(connector) = state.catalog_openai_connector(provider_id, config.kind()) {
        return Ok(connector);
    }
    let credential_kind = ProviderFactory::credential_kind(&config)
        .map_err(|error| validation("provider", &error.to_string()))?;
    let plaintext = match credential_kind {
        CredentialKind::None => None,
        CredentialKind::ApiKey | CredentialKind::ServiceAccountJson | CredentialKind::AwsStatic => {
            let stored = store
                .active_provider_credential_secret(provider_id)
                .await
                .map_err(map_catalog)?;
            let master_key = state
                .master_key
                .as_ref()
                .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))?;
            Some(
                master_key
                    .open(
                        &stored.encrypted,
                        &credential_aad(provider_id, stored.id, stored.version),
                    )
                    .map_err(|error| {
                        error!(%error, provider_id = %provider_id, "provider credential decryption failed");
                        Problem::internal()
                    })?,
            )
        }
    };
    let credential = catalog_credential(
        &config,
        plaintext.as_ref().map(|plaintext| plaintext.as_slice()),
    )
    .map_err(|error| validation("provider", &error.to_string()))?;
    ProviderFactory::create(config, credential)
        .await
        .map_err(|error| validation("provider", &error.to_string()))
}

fn provider_config(provider: &ProviderCatalogRecord) -> Result<ProviderConfig, String> {
    provider_config_fields(
        provider.kind,
        provider.endpoint.clone(),
        provider.cloud_region.clone(),
        provider.cloud_project.clone(),
        provider.deployment.clone(),
        provider.api_version.clone(),
        provider.auth_mode,
        provider.probe_model.clone(),
    )
}

#[allow(clippy::too_many_arguments)]
fn provider_config_fields(
    kind: ProviderKind,
    endpoint: Option<String>,
    cloud_region: Option<String>,
    cloud_project: Option<String>,
    deployment: Option<String>,
    api_version: Option<String>,
    auth_mode: olp_domain::ProviderAuthMode,
    probe_model: Option<String>,
) -> Result<ProviderConfig, String> {
    let required =
        |value: Option<String>, message: &'static str| value.ok_or_else(|| message.to_owned());
    Ok(match kind {
        ProviderKind::OpenAi => ProviderConfig::OpenAi { endpoint },
        ProviderKind::OpenAiCompatible => ProviderConfig::OpenAiCompatible {
            endpoint: required(endpoint, "OpenAI-compatible endpoint is missing")?,
        },
        ProviderKind::Anthropic => ProviderConfig::Anthropic {
            endpoint,
            api_version,
        },
        ProviderKind::Gemini => ProviderConfig::Gemini { endpoint },
        ProviderKind::VertexAi => ProviderConfig::VertexAi {
            project: required(cloud_project, "Vertex AI project is missing")?,
            location: required(cloud_region, "Vertex AI location is missing")?,
            probe_model: required(probe_model, "Vertex AI probe model is missing")?,
            auth_mode,
        },
        ProviderKind::Bedrock => ProviderConfig::Bedrock {
            region: required(cloud_region, "Bedrock AWS region is missing")?,
            auth_mode,
        },
        ProviderKind::AzureOpenAi => ProviderConfig::AzureOpenAi {
            endpoint: required(endpoint, "Azure OpenAI endpoint is missing")?,
            deployment: required(deployment, "Azure OpenAI deployment is missing")?,
            api_version: required(api_version, "Azure OpenAI API version is missing")?,
        },
    })
}

fn catalog_credential(
    config: &ProviderConfig,
    credential: Option<&[u8]>,
) -> Result<ProviderCredential, ProviderError> {
    match (ProviderFactory::credential_kind(config)?, credential) {
        (CredentialKind::None, _) | (_, None) => Ok(ProviderCredential::None),
        (CredentialKind::ApiKey, Some(credential)) => {
            let credential = std::str::from_utf8(credential).map_err(|_| {
                ProviderError::Credential("provider credential is not valid UTF-8".to_owned())
            })?;
            Ok(ProviderCredential::ApiKey(Zeroizing::new(
                credential.to_owned(),
            )))
        }
        (CredentialKind::ServiceAccountJson, Some(credential)) => {
            let credential = std::str::from_utf8(credential).map_err(|_| {
                ProviderError::Credential("provider credential is not valid UTF-8".to_owned())
            })?;
            Ok(ProviderCredential::ServiceAccountJson(Zeroizing::new(
                credential.to_owned(),
            )))
        }
        (CredentialKind::AwsStatic, Some(credential)) => Ok(ProviderCredential::AwsStatic(
            Zeroizing::new(credential.to_vec()),
        )),
    }
}
