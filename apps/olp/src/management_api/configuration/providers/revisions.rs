use axum::{
    Json,
    extract::{Path, Query, State},
    http::HeaderMap,
    response::Response,
};
use chrono::{DateTime, Utc};
use olp_domain::{ProviderAuthMode, ProviderKind};
use olp_storage::{ProviderRevisionDiff, ProviderRevisionRecord};
use serde::Serialize;
use utoipa::ToSchema;
use uuid::Uuid;

use crate::{
    ManagementState, Problem,
    management_api::{
        Permission, if_match, require_idempotency_key, require_mutation_session,
        require_permission, require_read_session,
    },
};

use super::{ProviderDetailResponse, models::ProviderModelListResponse};
use crate::management_api::configuration::common::{
    DiffQuery, PageQuery, map_configuration_resource, page, with_etag,
};

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ProviderRevisionResponse {
    pub id: Uuid,
    pub provider_id: Uuid,
    pub revision: i32,
    pub name: String,
    pub kind: ProviderKind,
    pub endpoint: Option<String>,
    pub cloud_region: Option<String>,
    pub cloud_project: Option<String>,
    pub deployment: Option<String>,
    pub api_version: Option<String>,
    pub auth_mode: ProviderAuthMode,
    pub connector_ready: bool,
    /// Historical metadata only. Restore never selects this credential.
    pub historical_credential_version: Option<i32>,
    pub source_etag: Uuid,
    pub activated_by: Uuid,
    pub activated_at: DateTime<Utc>,
    pub model_count: u64,
    pub enabled_model_count: u64,
    pub capability_count: u64,
    pub certified_capability_count: u64,
}

impl From<ProviderRevisionRecord> for ProviderRevisionResponse {
    fn from(value: ProviderRevisionRecord) -> Self {
        Self {
            id: value.id,
            provider_id: value.provider_id,
            revision: value.revision,
            name: value.name,
            kind: value.kind,
            endpoint: value.endpoint,
            cloud_region: value.cloud_region,
            cloud_project: value.cloud_project,
            deployment: value.deployment,
            api_version: value.api_version,
            auth_mode: value.auth_mode,
            connector_ready: value.connector_ready,
            historical_credential_version: value.credential_version,
            source_etag: value.source_etag,
            activated_by: value.activated_by,
            activated_at: value.activated_at,
            model_count: value.model_count,
            enabled_model_count: value.enabled_model_count,
            capability_count: value.capability_count,
            certified_capability_count: value.certified_capability_count,
        }
    }
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ProviderRevisionSummaryResponse {
    pub id: Uuid,
    pub provider_id: Uuid,
    pub revision: i32,
    pub name: String,
    pub kind: ProviderKind,
    pub connector_ready: bool,
    /// Historical metadata only. Restore never selects this credential.
    pub historical_credential_version: Option<i32>,
    pub activated_by: Uuid,
    pub activated_at: DateTime<Utc>,
    pub model_count: u64,
    pub enabled_model_count: u64,
    pub capability_count: u64,
    pub certified_capability_count: u64,
}

impl From<ProviderRevisionRecord> for ProviderRevisionSummaryResponse {
    fn from(value: ProviderRevisionRecord) -> Self {
        Self {
            id: value.id,
            provider_id: value.provider_id,
            revision: value.revision,
            name: value.name,
            kind: value.kind,
            connector_ready: value.connector_ready,
            historical_credential_version: value.credential_version,
            activated_by: value.activated_by,
            activated_at: value.activated_at,
            model_count: value.model_count,
            enabled_model_count: value.enabled_model_count,
            capability_count: value.capability_count,
            certified_capability_count: value.certified_capability_count,
        }
    }
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ProviderRevisionListResponse {
    pub items: Vec<ProviderRevisionSummaryResponse>,
    pub next_cursor: Option<String>,
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ProviderRevisionDiffResponse {
    pub from_revision: i32,
    pub to_revision: i32,
    pub name_changed: bool,
    pub endpoint_changed: bool,
    pub cloud_context_changed: bool,
    pub deployment_changed: bool,
    pub api_version_changed: bool,
    pub connector_changed: bool,
    pub credential_changed: bool,
    #[schema(max_items = 2000)]
    pub models_added: Vec<String>,
    #[schema(max_items = 2000)]
    pub models_removed: Vec<String>,
    #[schema(max_items = 2000)]
    pub models_changed: Vec<String>,
    #[schema(max_items = 32000)]
    pub capabilities_added: Vec<String>,
    #[schema(max_items = 32000)]
    pub capabilities_removed: Vec<String>,
}

impl From<ProviderRevisionDiff> for ProviderRevisionDiffResponse {
    fn from(value: ProviderRevisionDiff) -> Self {
        Self {
            from_revision: value.from_revision,
            to_revision: value.to_revision,
            name_changed: value.name_changed,
            endpoint_changed: value.endpoint_changed,
            cloud_context_changed: value.cloud_context_changed,
            deployment_changed: value.deployment_changed,
            api_version_changed: value.api_version_changed,
            connector_changed: value.connector_changed,
            credential_changed: value.credential_changed,
            models_added: value.models_added,
            models_removed: value.models_removed,
            models_changed: value.models_changed,
            capabilities_added: value.capabilities_added,
            capabilities_removed: value.capabilities_removed,
        }
    }
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ProviderRevisionRestoreResponse {
    pub provider: ProviderDetailResponse,
    /// Always false: historical credential material is never restored.
    pub credential_restored: bool,
}

#[utoipa::path(
    get,
    path = "/api/v1/providers/{provider_id}/revisions",
    tag = "providers",
    params(
        ("provider_id" = Uuid, Path),
        ("cursor" = Option<String>, Query),
        ("limit" = Option<u16>, Query, minimum = 1, maximum = 100)
    ),
    responses(
        (status = 200, body = ProviderRevisionListResponse),
        (status = 404, body = Problem)
    )
)]
pub(crate) async fn list_provider_revisions(
    State(state): State<ManagementState>,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<ProviderRevisionListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let page = state
        .store()
        .list_provider_revisions(provider_id, cursor, limit)
        .await
        .map_err(map_configuration_resource)?;
    Ok(Json(ProviderRevisionListResponse {
        items: page.items.into_iter().map(Into::into).collect(),
        next_cursor: page.next_cursor.map(|value| value.to_string()),
    }))
}

#[utoipa::path(
    get,
    path = "/api/v1/providers/{provider_id}/revisions/{revision_id}",
    tag = "providers",
    params(("provider_id" = Uuid, Path), ("revision_id" = Uuid, Path)),
    responses(
        (status = 200, body = ProviderRevisionResponse),
        (status = 404, body = Problem)
    )
)]
pub(crate) async fn get_provider_revision(
    State(state): State<ManagementState>,
    Path((provider_id, revision_id)): Path<(Uuid, Uuid)>,
    headers: HeaderMap,
) -> Result<Json<ProviderRevisionResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    Ok(Json(
        state
            .store()
            .get_provider_revision(provider_id, revision_id)
            .await
            .map_err(map_configuration_resource)?
            .into(),
    ))
}

#[utoipa::path(
    get,
    path = "/api/v1/providers/{provider_id}/revisions/{revision_id}/models",
    tag = "providers",
    params(
        ("provider_id" = Uuid, Path),
        ("revision_id" = Uuid, Path),
        ("cursor" = Option<String>, Query),
        ("limit" = Option<u16>, Query, minimum = 1, maximum = 100)
    ),
    responses(
        (status = 200, description = "Bounded historical provider model and capability page", body = ProviderModelListResponse),
        (status = 404, body = Problem)
    )
)]
pub(crate) async fn list_provider_revision_models(
    State(state): State<ManagementState>,
    Path((provider_id, revision_id)): Path<(Uuid, Uuid)>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<ProviderModelListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let page = state
        .store()
        .list_provider_revision_models(provider_id, revision_id, cursor, limit)
        .await
        .map_err(map_configuration_resource)?;
    Ok(Json(ProviderModelListResponse {
        items: page.items.into_iter().map(Into::into).collect(),
        next_cursor: page.next_cursor.map(|value| value.to_string()),
    }))
}

#[utoipa::path(
    get,
    path = "/api/v1/providers/{provider_id}/revisions/diff",
    tag = "providers",
    description = "Compares two immutable provider revisions. Each revision is limited to 2,000 models and 32,000 capability tuples; larger revisions fail with 422 instead of producing an unbounded response.",
    params(("provider_id" = Uuid, Path), ("from" = Uuid, Query), ("to" = Uuid, Query)),
    responses(
        (status = 200, body = ProviderRevisionDiffResponse),
        (status = 422, description = "Either revision exceeds the bounded diff ceiling", body = Problem),
        (status = 404, body = Problem)
    )
)]
pub(crate) async fn diff_provider_revisions(
    State(state): State<ManagementState>,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
    Query(query): Query<DiffQuery>,
) -> Result<Json<ProviderRevisionDiffResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    Ok(Json(
        state
            .store()
            .diff_provider_revisions(provider_id, query.from, query.to)
            .await
            .map_err(map_configuration_resource)?
            .into(),
    ))
}

#[utoipa::path(
    post,
    path = "/api/v1/providers/{provider_id}/revisions/{revision_id}/restore-as-draft",
    tag = "providers",
    params(
        ("provider_id" = Uuid, Path),
        ("revision_id" = Uuid, Path),
        ("If-Match" = String, Header, description = "Current provider draft ETag"),
        ("Idempotency-Key" = String, Header)
    ),
    responses(
        (status = 200, description = "Historical non-secret configuration restored as a draft; current credential selection is preserved", body = ProviderRevisionRestoreResponse),
        (status = 409, description = "Idempotency-Key was already used", body = Problem),
        (status = 412, body = Problem),
        (status = 422, body = Problem)
    )
)]
pub(crate) async fn restore_provider_revision(
    State(state): State<ManagementState>,
    Path((provider_id, revision_id)): Path<(Uuid, Uuid)>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageProviders)?;
    let restored = state
        .store()
        .restore_provider_revision_as_draft(
            provider_id,
            revision_id,
            if_match(&headers)?,
            principal.user_id,
            require_idempotency_key(&headers)?,
        )
        .await
        .map_err(map_configuration_resource)?;
    let etag = restored.etag;
    with_etag(
        Json(ProviderRevisionRestoreResponse {
            provider: restored.into(),
            credential_restored: false,
        }),
        etag,
    )
}
