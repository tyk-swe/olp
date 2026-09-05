use axum::{
    Json,
    extract::{Path, Query, State, rejection::JsonRejection},
    http::{HeaderMap, StatusCode},
    response::Response,
};
use chrono::{DateTime, Utc};
use olp_db::{
    configuration::resources::ProviderRecord, configuration::resources::UpdateProvider,
    idempotency::operations, store::Store,
};
use olp_engine::domain::{
    auth::Permission,
    provider::ProviderAuthMode,
    provider_configuration::{Configuration, validate},
    routing::provider::ProviderKind,
};
use olp_engine::providers::{factory::assembly::Factory, http_egress::EgressPolicy};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;
use uuid::Uuid;

use {
    crate::{
        application::provider_fields::ProviderConfigFields,
        management::{
            error_mapping::map_configuration,
            idempotency::{MutationReply, ReplayableMutation},
            json_payload::json_payload,
            pagination::{PageQuery, page},
            permissions::require_permission,
            preconditions::{if_match, with_etag},
            principal::{MutationPrincipal, ReadPrincipal},
            provider_connector::provider_connector,
            state::ManagementState,
        },
        public_http::problem::{FieldErrorCodes, FieldErrors, Problem},
    },
    olp_engine::providers::factory::input::provider_config,
};

use super::credentials::ProviderMutationResponse;
use super::record_violations;
use crate::management::provenance::Provenance;

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ProviderSummaryResponse {
    pub id: Uuid,
    pub name: String,
    pub kind: ProviderKind,
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
    /// Email of the operator who created the provider.
    pub created_by_email: Option<String>,
}

/// The summary is the detail minus its connector and credential fields, so
/// the record-to-response mapping is written once, on the detail.
impl From<ProviderDetailResponse> for ProviderSummaryResponse {
    fn from(value: ProviderDetailResponse) -> Self {
        Self {
            id: value.id,
            name: value.name,
            kind: value.kind,
            state: value.state,
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
            created_by_email: value.created_by_email,
        }
    }
}

impl From<ProviderRecord> for ProviderSummaryResponse {
    fn from(value: ProviderRecord) -> Self {
        ProviderDetailResponse::from(value).into()
    }
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ProviderDetailResponse {
    pub id: Uuid,
    pub name: String,
    pub kind: ProviderKind,
    pub state: String,
    pub endpoint: Option<String>,
    pub cloud_region: Option<String>,
    pub cloud_project: Option<String>,
    pub deployment: Option<String>,
    pub api_version: Option<String>,
    pub auth_mode: ProviderAuthMode,
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
    /// Email of the operator who created the provider.
    pub created_by_email: Option<String>,
}

impl From<ProviderRecord> for ProviderDetailResponse {
    fn from(value: ProviderRecord) -> Self {
        Self {
            id: value.id,
            name: value.name,
            kind: value.kind,
            state: value.state.to_string(),
            endpoint: value.endpoint,
            cloud_region: value.cloud_region,
            cloud_project: value.cloud_project,
            deployment: value.deployment,
            api_version: value.api_version,
            auth_mode: value.auth_mode,
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
            created_by_email: value.created_by_email,
        }
    }
}

pub(super) async fn load_provider_detail(
    store: &Store,
    provider_id: Uuid,
) -> Result<ProviderDetailResponse, Problem> {
    store
        .get_provider(provider_id)
        .await
        .map(Into::into)
        .map_err(map_configuration)
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
        PageQuery,
    ),
    responses(
        (status = 200, body = ProviderListResponse),
        (status = 400, description = "Malformed query parameters, or an invalid cursor or page size", body = Problem),
        (status = 401, body = Problem),
        (status = 403, body = Problem)
    )
)]
pub(crate) async fn list_providers(
    State(state): State<ManagementState>,
    Query(query): Query<PageQuery>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<ProviderListResponse>, Problem> {
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let page = state
        .store()
        .list_providers(cursor, limit)
        .await
        .map_err(map_configuration)?;
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
    State(state): State<ManagementState>,
    Path(provider_id): Path<Uuid>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ReadConfiguration)?;
    let provider = load_provider_detail(state.store(), provider_id).await?;
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
    pub auth_mode: ProviderAuthMode,
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
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
    MutationPrincipal(principal): MutationPrincipal,
    payload: Result<Json<UpdateProviderRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ManageProviders)?;
    let request = json_payload(payload)?;
    let store = state.store().with_provenance(&provenance);
    let current = store
        .get_provider(provider_id)
        .await
        .map_err(map_configuration)?;
    validate_provider_update(&current, &request, &state.provider_egress_policy)?;
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
                auth_mode: request.auth_mode,
            },
            principal.user_id,
        )
        .await
        .map_err(map_configuration)?;
    let provider = load_provider_detail(&store, provider_id).await?;
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
        (status = 400, description = "Idempotency-Key is missing or invalid", body = Problem),
        (status = 409, description = "Provider is still referenced by an active route, or the Idempotency-Key was already used for a different request", body = Problem),
        (status = 412, body = Problem)
    )
)]
pub(crate) async fn disable_provider(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    Path(provider_id): Path<Uuid>,
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
        operations::PROVIDER_DISABLE,
        &headers,
        &ProviderMutationFingerprint {
            provider_id,
            expected_etag,
        },
    )?
    .run(|key| async move {
        let result = state
            .store()
            .with_provenance(provenance)
            .disable_provider(provider_id, expected_etag, principal.user_id, &key)
            .await
            .map_err(map_configuration)?;
        Ok(MutationReply {
            status: StatusCode::OK,
            body: ProviderMutationResponse {
                provider_id,
                etag: result.etag,
                credential_id: None,
                credential_version: None,
                runtime_generation: result.release.as_ref().map(Into::into),
            },
            etag: Some(result.etag),
            location: None,
        })
    })
    .await
}

#[derive(Serialize)]
pub(super) struct ProviderMutationFingerprint {
    pub provider_id: Uuid,
    pub expected_etag: Uuid,
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
    responses(
        (status = 200, body = ProviderDetailResponse),
        (status = 400, description = "Idempotency-Key is missing or invalid", body = Problem),
        (status = 409, description = "Idempotency-Key was already used or is in progress", body = Problem),
        (status = 412, body = Problem)
    )
)]
pub(crate) async fn restore_provider_as_draft(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    Path(provider_id): Path<Uuid>,
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
        operations::PROVIDER_RESTORE_AS_DRAFT,
        &headers,
        &ProviderMutationFingerprint {
            provider_id,
            expected_etag,
        },
    )?
    .run(|key| async move {
        let store = state.store().with_provenance(provenance);
        let etag = store
            .restore_provider_as_draft(provider_id, expected_etag, principal.user_id, &key)
            .await
            .map_err(map_configuration)?;
        let provider = load_provider_detail(&store, provider_id).await?;
        Ok(MutationReply {
            status: StatusCode::OK,
            body: provider,
            etag: Some(etag),
            location: None,
        })
    })
    .await
}

fn validate_provider_update(
    provider: &ProviderRecord,
    request: &UpdateProviderRequest,
    egress_policy: &EgressPolicy,
) -> Result<(), Problem> {
    if request.auth_mode != provider.auth_mode {
        return Err(Problem::field_validation(
            "auth_mode",
            "Provider authentication mode is immutable; create a separate provider to change identity mode.",
        ));
    }

    let mut errors = FieldErrors::new();
    let mut codes = FieldErrorCodes::new();
    record_violations(
        validate(Configuration {
            kind: provider.kind,
            auth_mode: request.auth_mode,
            endpoint: request.endpoint.as_deref(),
            cloud_region: request.cloud_region.as_deref(),
            cloud_project: request.cloud_project.as_deref(),
            deployment: request.deployment.as_deref(),
            api_version: request.api_version.as_deref(),
            model: provider.probe_model.as_deref(),
            credential_present: Some(provider.draft_credential_id.is_some()),
        }),
        &mut errors,
        &mut codes,
    );
    if !errors.is_empty() {
        return Err(Problem::coded_validation(errors, codes));
    }

    let config = provider_config(
        ProviderConfigFields {
            kind: provider.kind,
            endpoint: request.endpoint.as_deref(),
            cloud_region: request.cloud_region.as_deref(),
            cloud_project: request.cloud_project.as_deref(),
            deployment: request.deployment.as_deref(),
            api_version: request.api_version.as_deref(),
            auth_mode: request.auth_mode,
            probe_model: provider.probe_model.as_deref(),
        }
        .into(),
    )
    .map_err(|error| Problem::field_validation("provider", error.to_string()))?;
    Factory::validate(&config, egress_policy)
        .map_err(|error| Problem::field_validation("provider", error.to_string()))
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
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
    MutationPrincipal(principal): MutationPrincipal,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ManageProviders)?;
    let store = state.store().with_provenance(&provenance);
    let expected_etag = if_match(&headers)?;
    let provider = store
        .get_provider(provider_id)
        .await
        .map_err(map_configuration)?;
    if provider.etag != expected_etag {
        return Err(map_configuration(
            olp_db::configuration::error::Error::PreconditionFailed,
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
        .map_err(map_configuration)?;
    if !succeeded {
        return Err(Problem::field_validation("provider", detail));
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
