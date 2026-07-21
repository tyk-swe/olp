use axum::{
    Json,
    extract::{Path, Query, State, rejection::JsonRejection},
    http::HeaderMap,
    response::Response,
};
use chrono::{DateTime, Utc};
use olp_domain::ProviderKind;
use olp_providers::ProviderFactory;
use olp_storage::{ProviderRecord, UpdateProvider};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;
use uuid::Uuid;

use crate::{
    ApiState, Problem,
    management_api::{
        Permission, common::RuntimeGenerationResponse, if_match, require_idempotency_key,
        require_mutation_session, require_permission, require_read_session, require_store,
    },
    provider_adapter::{ProviderConfigFields, provider_config, provider_connector},
};

use super::credentials::ProviderMutationResponse;
use crate::management_api::configuration::common::{
    PageQuery, json, map_configuration_resource, page, validation, with_etag,
};

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ProviderSummaryResponse {
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

impl From<ProviderRecord> for ProviderSummaryResponse {
    fn from(value: ProviderRecord) -> Self {
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
pub(crate) struct ProviderDetailResponse {
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

impl From<ProviderRecord> for ProviderDetailResponse {
    fn from(value: ProviderRecord) -> Self {
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
pub(crate) struct ProviderListResponse {
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
pub(crate) async fn list_providers(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<ProviderListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let page = require_store(&state)?
        .list_providers(cursor, limit)
        .await
        .map_err(map_configuration_resource)?;
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
    responses((status = 200, body = ProviderDetailResponse), (status = 404, body = Problem))
)]
pub(crate) async fn get_provider(
    State(state): State<ApiState>,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let provider: ProviderDetailResponse = require_store(&state)?
        .get_provider(provider_id)
        .await
        .map_err(map_configuration_resource)?
        .into();
    let etag = provider.etag;
    with_etag(Json(provider), etag)
}

#[derive(Debug, Deserialize, ToSchema)]
pub(crate) struct UpdateProviderRequest {
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
    responses((status = 200, body = ProviderDetailResponse), (status = 412, body = Problem), (status = 422, body = Problem))
)]
pub(crate) async fn update_provider(
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
        .get_provider(provider_id)
        .await
        .map_err(map_configuration_resource)?;
    validate_provider_update(&current, &request)
        .map_err(|detail| validation("provider", &detail))?;
    let etag = store
        .update_provider(
            provider_id,
            if_match(&headers)?,
            &UpdateProvider {
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
        .map_err(map_configuration_resource)?;
    let provider: ProviderDetailResponse = store
        .get_provider(provider_id)
        .await
        .map_err(map_configuration_resource)?
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
pub(crate) async fn disable_provider(
    State(state): State<ApiState>,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageProviders)?;
    let result = require_store(&state)?
        .disable_provider(
            provider_id,
            if_match(&headers)?,
            principal.user_id,
            require_idempotency_key(&headers)?,
        )
        .await
        .map_err(map_configuration_resource)?;
    with_etag(
        Json(ProviderMutationResponse {
            provider_id,
            etag: result.etag,
            credential_id: None,
            credential_version: None,
            runtime_generation: result.release.map(|release| RuntimeGenerationResponse {
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
    responses((status = 200, body = ProviderDetailResponse), (status = 412, body = Problem))
)]
pub(crate) async fn restore_provider_as_draft(
    State(state): State<ApiState>,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageProviders)?;
    let store = require_store(&state)?;
    let etag = store
        .restore_provider_as_draft(
            provider_id,
            if_match(&headers)?,
            principal.user_id,
            require_idempotency_key(&headers)?,
        )
        .await
        .map_err(map_configuration_resource)?;
    let provider: ProviderDetailResponse = store
        .get_provider(provider_id)
        .await
        .map_err(map_configuration_resource)?
        .into();
    with_etag(Json(provider), etag)
}

fn validate_provider_update(
    provider: &ProviderRecord,
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
    let config = provider_config(ProviderConfigFields {
        kind,
        endpoint: request.endpoint.as_deref(),
        cloud_region: request.cloud_region.as_deref(),
        cloud_project: request.cloud_project.as_deref(),
        deployment: request.deployment.as_deref(),
        api_version: request.api_version.as_deref(),
        auth_mode: request
            .auth_mode
            .parse()
            .map_err(|_| "Provider authentication mode is invalid".to_owned())?,
        probe_model: Some(&model),
    })
    .map_err(|error| error.to_string())?;
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
pub(crate) struct ProbeResponse {
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
pub(crate) async fn probe_provider(
    State(state): State<ApiState>,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageProviders)?;
    let store = require_store(&state)?;
    let expected_etag = if_match(&headers)?;
    let provider = store
        .get_provider(provider_id)
        .await
        .map_err(map_configuration_resource)?;
    if provider.etag != expected_etag {
        return Err(map_configuration_resource(
            olp_storage::ConfigurationError::PreconditionFailed,
        ));
    }
    let connector = provider_connector(&state, provider_id).await?;
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
        .map_err(map_configuration_resource)?;
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
