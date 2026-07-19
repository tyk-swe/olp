use std::{collections::BTreeSet, fmt};

use axum::{
    Json, Router,
    extract::{Path, Query, State, rejection::JsonRejection},
    http::{HeaderMap, HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
    routing::{get, patch, post},
};
use chrono::{DateTime, Utc};
use futures::{StreamExt as _, stream};
use olp_domain::ProviderKind;
use olp_providers::{
    CapabilityCertificationEvidence, CompatibleCapability, CompatibleCapabilityCertificationError,
    CredentialKind, ProviderConfig, ProviderCredential, ProviderError, ProviderFacade,
    ProviderFactory, certifiable_capabilities,
};
use olp_storage::{
    ApiKeyCatalogRecord, CapabilityCertificationOutcome, CapabilityRecord, CatalogError,
    CredentialVersionRecord, DiscoveredModelInput, ProviderCatalogRecord,
    ProviderModelInventoryRecord, ProviderModelRecord, ProviderRevisionCatalogRecord,
    ProviderRevisionDiff, ReplaceRouteDraftCatalogInput, ReplayableIdempotency,
    RotateApiKeyCatalogInput, RotateCredentialInput, RouteCatalogRecord, RouteDraftCatalogRecord,
    RouteRevisionCatalogRecord, RouteRevisionDiff, RouteSimulation, RouteSimulationTarget,
    RouteTargetRecord, UpdateApiKeyCatalogInput, UpdateProviderCatalog, credential_aad,
    idempotency_fingerprint, idempotency_secret_digest,
};
use serde::{Deserialize, Serialize};
use tracing::error;
use utoipa::{OpenApi, ToSchema};
use uuid::Uuid;
use zeroize::Zeroizing;

use crate::{
    ApiState, FieldErrors, Problem,
    management::{
        Permission, WriteOnlySecret, idempotency_http_response, if_match, require_idempotency_key,
        require_mutation_session, require_permission, require_read_session, require_store,
    },
};

pub fn router() -> Router<ApiState> {
    Router::new()
        .route(
            "/api/v1/provider-kinds/{provider_kind}/capabilities",
            get(list_provider_kind_capabilities),
        )
        .route("/api/v1/providers", get(list_providers))
        .route(
            "/api/v1/provider-models",
            get(list_provider_model_inventory),
        )
        .route(
            "/api/v1/providers/{provider_id}",
            get(get_provider).patch(update_provider),
        )
        .route(
            "/api/v1/providers/{provider_id}/disable",
            post(disable_provider),
        )
        .route(
            "/api/v1/providers/{provider_id}/restore-as-draft",
            post(restore_provider_as_draft),
        )
        .route(
            "/api/v1/providers/{provider_id}/revisions",
            get(list_provider_revisions),
        )
        .route(
            "/api/v1/providers/{provider_id}/revisions/diff",
            get(diff_provider_revisions),
        )
        .route(
            "/api/v1/providers/{provider_id}/revisions/{revision_id}",
            get(get_provider_revision),
        )
        .route(
            "/api/v1/providers/{provider_id}/revisions/{revision_id}/models",
            get(list_provider_revision_models),
        )
        .route(
            "/api/v1/providers/{provider_id}/revisions/{revision_id}/restore-as-draft",
            post(restore_provider_revision),
        )
        .route(
            "/api/v1/providers/{provider_id}/credentials",
            get(list_provider_credentials).post(rotate_provider_credential),
        )
        .route(
            "/api/v1/providers/{provider_id}/credentials/{credential_id}/revoke",
            post(revoke_provider_credential),
        )
        .route(
            "/api/v1/providers/{provider_id}/probe",
            post(probe_provider),
        )
        .route(
            "/api/v1/providers/{provider_id}/discovery",
            post(discover_provider_models),
        )
        .route(
            "/api/v1/providers/{provider_id}/models/{model_id}",
            patch(set_provider_model),
        )
        .route(
            "/api/v1/providers/{provider_id}/models",
            get(list_provider_models),
        )
        .route(
            "/api/v1/providers/{provider_id}/models/{model_id}/certify",
            post(certify_provider_model),
        )
        .route("/api/v1/route-drafts", get(list_route_drafts))
        .route(
            "/api/v1/route-drafts/{draft_id}",
            get(get_route_draft)
                .put(replace_route_draft)
                .delete(delete_route_draft),
        )
        .route(
            "/api/v1/route-drafts/{draft_id}/simulate",
            post(simulate_route_draft),
        )
        .route("/api/v1/routes", get(list_routes))
        .route("/api/v1/routes/{route_id}", get(get_route))
        .route(
            "/api/v1/routes/{route_id}/revisions",
            get(list_route_revisions),
        )
        .route(
            "/api/v1/routes/{route_id}/revisions/diff",
            get(diff_route_revisions),
        )
        .route(
            "/api/v1/routes/{route_id}/revisions/{revision_id}",
            get(get_route_revision),
        )
        .route(
            "/api/v1/routes/{route_id}/revisions/{revision_id}/restore-as-draft",
            post(restore_route_revision),
        )
        .route("/api/v1/api-keys", get(list_api_keys))
        .route(
            "/api/v1/api-keys/{api_key_id}",
            get(get_api_key).patch(update_api_key),
        )
        .route("/api/v1/api-keys/{api_key_id}/rotate", post(rotate_api_key))
}

#[derive(OpenApi)]
#[openapi(
    paths(
        list_provider_kind_capabilities,
        list_providers,
        list_provider_model_inventory,
        get_provider,
        list_provider_models,
        update_provider,
        disable_provider,
        restore_provider_as_draft,
        list_provider_revisions,
        get_provider_revision,
        list_provider_revision_models,
        diff_provider_revisions,
        restore_provider_revision,
        list_provider_credentials,
        rotate_provider_credential,
        revoke_provider_credential,
        probe_provider,
        discover_provider_models,
        set_provider_model,
        certify_provider_model,
        list_route_drafts,
        get_route_draft,
        replace_route_draft,
        delete_route_draft,
        simulate_route_draft,
        list_routes,
        get_route,
        list_route_revisions,
        get_route_revision,
        diff_route_revisions,
        restore_route_revision,
        list_api_keys,
        get_api_key,
        update_api_key,
        rotate_api_key
    ),
    components(schemas(
        PageQuery,
        ProviderCapabilityOptionsResponse,
        CapabilityResponse,
        ProviderModelResponse,
        ProviderModelListResponse,
        ProviderModelInventoryResponse,
        ProviderModelInventoryListResponse,
        ProviderSummaryResponse,
        ProviderCatalogResponse,
        ProviderListResponse,
        ProviderRevisionSummaryResponse,
        ProviderRevisionResponse,
        ProviderRevisionListResponse,
        ProviderRevisionDiffResponse,
        ProviderRevisionRestoreResponse,
        UpdateProviderRequest,
        CredentialResponse,
        CredentialListResponse,
        RotateCredentialRequest,
        ProviderMutationResponse,
        ProbeResponse,
        CapabilityInput,
        DiscoveredModelRequest,
        DiscoverModelsRequest,
        SetModelRequest,
        CapabilityCertificationItemResponse,
        CapabilityCertificationResponse,
        RouteTargetCatalogResponse,
        RouteDraftCatalogResponse,
        RouteDraftListResponse,
        ReplaceRouteDraftRequest,
        ReplaceRouteTargetRequest,
        SimulateRouteRequest,
        RouteSimulationTargetResponse,
        RouteSimulationResponse,
        RouteCatalogResponse,
        RouteListResponse,
        RouteRevisionResponse,
        RouteRevisionListResponse,
        RouteRevisionDiffResponse,
        ApiKeyCatalogResponse,
        ApiKeyListResponse,
        UpdateApiKeyRequest,
        ApiKeyMutationResponse,
        RotateApiKeyResponse,
        RuntimeGenerationCatalogResponse,
        Problem
    )),
    tags(
        (name = "providers"),
        (name = "routes"),
        (name = "api-keys")
    )
)]
pub struct CatalogApiDoc;

#[derive(Debug, Deserialize, ToSchema)]
pub struct PageQuery {
    pub cursor: Option<String>,
    pub limit: Option<u16>,
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub struct ProviderCapabilityOptionsResponse {
    pub provider_kind: String,
    /// Capability tuples with a safe server-owned certification path for this
    /// provider kind. Catalog validation may support additional future tuples.
    pub capabilities: Vec<CapabilityInput>,
}

#[utoipa::path(
    get,
    path = "/api/v1/provider-kinds/{provider_kind}/capabilities",
    tag = "providers",
    params(("provider_kind" = String, Path, description = "Canonical provider kind")),
    responses(
        (status = 200, body = ProviderCapabilityOptionsResponse),
        (status = 400, body = Problem),
        (status = 401, body = Problem),
        (status = 403, body = Problem)
    )
)]
async fn list_provider_kind_capabilities(
    State(state): State<ApiState>,
    Path(provider_kind): Path<String>,
    headers: HeaderMap,
) -> Result<Json<ProviderCapabilityOptionsResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let provider_kind = provider_kind.parse::<ProviderKind>().map_err(|_| {
        Problem::bad_request(
            "invalid_provider_kind",
            "The provider kind is not supported by this installation.",
        )
    })?;

    Ok(Json(ProviderCapabilityOptionsResponse {
        provider_kind: provider_kind.as_str().to_owned(),
        capabilities: certifiable_capabilities(provider_kind)
            .map(|(operation, surface, mode)| CapabilityInput {
                operation: operation.as_str().to_owned(),
                surface: surface.as_str().to_owned(),
                mode: mode.as_str().to_owned(),
            })
            .collect(),
    }))
}

#[derive(Debug, Deserialize, ToSchema)]
pub struct ProviderModelInventoryQuery {
    pub cursor: Option<String>,
    pub limit: Option<u16>,
    pub enabled: Option<bool>,
}

fn page(query: PageQuery) -> Result<(Option<Uuid>, i64), Problem> {
    let cursor = query
        .cursor
        .map(|value| {
            Uuid::parse_str(&value).map_err(|_| {
                Problem::bad_request("invalid_cursor", "The pagination cursor is invalid.")
            })
        })
        .transpose()?;
    let limit = query.limit.unwrap_or(50);
    if !(1..=100).contains(&limit) {
        return Err(Problem::bad_request(
            "invalid_page_size",
            "Page size must be between 1 and 100.",
        ));
    }
    Ok((cursor, i64::from(limit)))
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub struct CapabilityResponse {
    pub operation: String,
    pub surface: String,
    pub mode: String,
    pub source: String,
    pub certified_at: Option<DateTime<Utc>>,
}

impl From<CapabilityRecord> for CapabilityResponse {
    fn from(value: CapabilityRecord) -> Self {
        Self {
            operation: value.operation.to_string(),
            surface: value.surface.to_string(),
            mode: value.mode.to_string(),
            source: value.source.to_string(),
            certified_at: value.certified_at,
        }
    }
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub struct ProviderModelResponse {
    pub id: Uuid,
    pub upstream_model: String,
    pub display_name: String,
    pub enabled: bool,
    pub discovered_at: Option<DateTime<Utc>>,
    pub capabilities: Vec<CapabilityResponse>,
}

impl From<ProviderModelRecord> for ProviderModelResponse {
    fn from(value: ProviderModelRecord) -> Self {
        Self {
            id: value.id,
            upstream_model: value.upstream_model,
            display_name: value.display_name,
            enabled: value.enabled,
            discovered_at: value.discovered_at,
            capabilities: value.capabilities.into_iter().map(Into::into).collect(),
        }
    }
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub struct ProviderModelListResponse {
    pub items: Vec<ProviderModelResponse>,
    pub next_cursor: Option<String>,
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub struct ProviderModelInventoryResponse {
    pub provider_id: Uuid,
    pub provider_name: String,
    pub provider_kind: String,
    pub model: ProviderModelResponse,
}

impl From<ProviderModelInventoryRecord> for ProviderModelInventoryResponse {
    fn from(value: ProviderModelInventoryRecord) -> Self {
        Self {
            provider_id: value.provider_id,
            provider_name: value.provider_name,
            provider_kind: value.provider_kind.to_string(),
            model: value.model.into(),
        }
    }
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub struct ProviderModelInventoryListResponse {
    pub items: Vec<ProviderModelInventoryResponse>,
    pub next_cursor: Option<String>,
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub struct ProviderSummaryResponse {
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
pub struct ProviderCatalogResponse {
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
pub struct ProviderListResponse {
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
async fn list_providers(
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
    path = "/api/v1/provider-models",
    tag = "providers",
    params(
        ("cursor" = Option<String>, Query),
        ("limit" = Option<u16>, Query, minimum = 1, maximum = 100),
        ("enabled" = Option<bool>, Query, description = "Optional enabled-state filter")
    ),
    responses(
        (status = 200, description = "Bounded cross-provider model and capability page", body = ProviderModelInventoryListResponse)
    )
)]
async fn list_provider_model_inventory(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<ProviderModelInventoryQuery>,
) -> Result<Json<ProviderModelInventoryListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let enabled = query.enabled;
    let (cursor, limit) = page(PageQuery {
        cursor: query.cursor,
        limit: query.limit,
    })?;
    let page = require_store(&state)?
        .list_provider_model_inventory_catalog(cursor, limit, enabled)
        .await
        .map_err(map_catalog)?;
    Ok(Json(ProviderModelInventoryListResponse {
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
async fn get_provider(
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

#[utoipa::path(
    get,
    path = "/api/v1/providers/{provider_id}/models",
    tag = "providers",
    params(
        ("provider_id" = Uuid, Path),
        ("cursor" = Option<String>, Query),
        ("limit" = Option<u16>, Query, minimum = 1, maximum = 100)
    ),
    responses(
        (status = 200, description = "Bounded provider model and capability page", body = ProviderModelListResponse),
        (status = 404, body = Problem)
    )
)]
async fn list_provider_models(
    State(state): State<ApiState>,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<ProviderModelListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let page = require_store(&state)?
        .list_provider_models_catalog(provider_id, cursor, limit)
        .await
        .map_err(map_catalog)?;
    Ok(Json(ProviderModelListResponse {
        items: page.items.into_iter().map(Into::into).collect(),
        next_cursor: page.next_cursor.map(|value| value.to_string()),
    }))
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub struct ProviderRevisionResponse {
    pub id: Uuid,
    pub provider_id: Uuid,
    pub revision: i32,
    pub name: String,
    pub kind: String,
    pub endpoint: Option<String>,
    pub cloud_region: Option<String>,
    pub cloud_project: Option<String>,
    pub deployment: Option<String>,
    pub api_version: Option<String>,
    pub auth_mode: String,
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

impl From<ProviderRevisionCatalogRecord> for ProviderRevisionResponse {
    fn from(value: ProviderRevisionCatalogRecord) -> Self {
        Self {
            id: value.id,
            provider_id: value.provider_id,
            revision: value.revision,
            name: value.name,
            kind: value.kind.to_string(),
            endpoint: value.endpoint,
            cloud_region: value.cloud_region,
            cloud_project: value.cloud_project,
            deployment: value.deployment,
            api_version: value.api_version,
            auth_mode: value.auth_mode.to_string(),
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
pub struct ProviderRevisionSummaryResponse {
    pub id: Uuid,
    pub provider_id: Uuid,
    pub revision: i32,
    pub name: String,
    pub kind: String,
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

impl From<ProviderRevisionCatalogRecord> for ProviderRevisionSummaryResponse {
    fn from(value: ProviderRevisionCatalogRecord) -> Self {
        Self {
            id: value.id,
            provider_id: value.provider_id,
            revision: value.revision,
            name: value.name,
            kind: value.kind.to_string(),
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
pub struct ProviderRevisionListResponse {
    pub items: Vec<ProviderRevisionSummaryResponse>,
    pub next_cursor: Option<String>,
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub struct ProviderRevisionDiffResponse {
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
pub struct ProviderRevisionRestoreResponse {
    pub provider: ProviderCatalogResponse,
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
async fn list_provider_revisions(
    State(state): State<ApiState>,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<ProviderRevisionListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let page = require_store(&state)?
        .list_provider_revisions_catalog(provider_id, cursor, limit)
        .await
        .map_err(map_catalog)?;
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
async fn get_provider_revision(
    State(state): State<ApiState>,
    Path((provider_id, revision_id)): Path<(Uuid, Uuid)>,
    headers: HeaderMap,
) -> Result<Json<ProviderRevisionResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    Ok(Json(
        require_store(&state)?
            .get_provider_revision_catalog(provider_id, revision_id)
            .await
            .map_err(map_catalog)?
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
async fn list_provider_revision_models(
    State(state): State<ApiState>,
    Path((provider_id, revision_id)): Path<(Uuid, Uuid)>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<ProviderModelListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let page = require_store(&state)?
        .list_provider_revision_models_catalog(provider_id, revision_id, cursor, limit)
        .await
        .map_err(map_catalog)?;
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
async fn diff_provider_revisions(
    State(state): State<ApiState>,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
    Query(query): Query<DiffQuery>,
) -> Result<Json<ProviderRevisionDiffResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    Ok(Json(
        require_store(&state)?
            .diff_provider_revisions_catalog(provider_id, query.from, query.to)
            .await
            .map_err(map_catalog)?
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
async fn restore_provider_revision(
    State(state): State<ApiState>,
    Path((provider_id, revision_id)): Path<(Uuid, Uuid)>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageProviders)?;
    let restored = require_store(&state)?
        .restore_provider_revision_as_draft(
            provider_id,
            revision_id,
            if_match(&headers)?,
            principal.user_id,
            require_idempotency_key(&headers)?,
        )
        .await
        .map_err(map_catalog)?;
    let etag = restored.etag;
    with_etag(
        Json(ProviderRevisionRestoreResponse {
            provider: restored.into(),
            credential_restored: false,
        }),
        etag,
    )
}

#[derive(Debug, Deserialize, ToSchema)]
pub struct UpdateProviderRequest {
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
async fn update_provider(
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
async fn disable_provider(
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
async fn restore_provider_as_draft(
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
        ProviderKind::OpenAiCompatible | ProviderKind::AnthropicCompatible => {
            require_auth_mode(request, "api_key")?;
            reject_cloud_fields(request)?;
            if request.endpoint.is_none() {
                let label = if kind == ProviderKind::OpenAiCompatible {
                    "OpenAI"
                } else {
                    "Anthropic"
                };
                return Err(format!("An {label}-compatible HTTPS endpoint is required"));
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
pub struct CredentialResponse {
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
pub struct CredentialListResponse {
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
async fn list_provider_credentials(
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
pub struct RotateCredentialRequest {
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
pub struct RuntimeGenerationCatalogResponse {
    pub id: Uuid,
    pub sequence: i64,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct ProviderMutationResponse {
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
async fn rotate_provider_credential(
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
async fn revoke_provider_credential(
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
pub struct ProbeResponse {
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
async fn probe_provider(
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
        return Err(map_catalog(CatalogError::PreconditionFailed));
    }
    let connector = catalog_connector(&state, provider_id).await?;
    // Configuration-only checks are intentionally not accepted as activation
    // evidence. A probe always performs a bounded credentialed upstream call,
    // and persistence binds the result to the exact ETag captured above.
    let probe = match (provider.kind, provider.probe_model.as_deref()) {
        (ProviderKind::AnthropicCompatible, Some(model)) => {
            connector.probe_model(model).await.map(|()| None)
        }
        _ => connector
            .discover_models()
            .await
            .map(|models| Some(models.len())),
    };
    let (succeeded, detail, discovered_models) = match probe {
        Ok(discovered_models) => (
            true,
            if discovered_models.is_none() {
                "Credentialed Messages request succeeded.".to_owned()
            } else {
                "Credentialed connector request succeeded.".to_owned()
            },
            discovered_models,
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

async fn catalog_connector(state: &ApiState, provider_id: Uuid) -> Result<ProviderFacade, Problem> {
    let store = require_store(state)?;
    let provider = store
        .get_provider_catalog(provider_id)
        .await
        .map_err(map_catalog)?;
    let config = provider_config(&provider).map_err(|detail| validation("provider", &detail))?;
    if let Some(connector) = state.catalog_openai_connector(provider_id, config.kind()) {
        return Ok(connector);
    }
    if let Some(connector) = state.catalog_anthropic_connector(provider_id, config.kind()) {
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
        ProviderKind::AnthropicCompatible => ProviderConfig::AnthropicCompatible {
            endpoint: required(endpoint, "Anthropic-compatible endpoint is missing")?,
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

#[derive(Clone, Debug, Deserialize, Serialize, ToSchema)]
pub struct CapabilityInput {
    pub operation: String,
    pub surface: String,
    pub mode: String,
}

fn capability_record(input: CapabilityInput) -> Result<CapabilityRecord, Problem> {
    Ok(CapabilityRecord {
        operation: input
            .operation
            .parse()
            .map_err(|_| validation("capabilities", "A reviewed operation is invalid."))?,
        surface: input
            .surface
            .parse()
            .map_err(|_| validation("capabilities", "A reviewed surface is invalid."))?,
        mode: input
            .mode
            .parse()
            .map_err(|_| validation("capabilities", "A reviewed mode is invalid."))?,
        source: olp_domain::CapabilitySource::Declared,
        certified_at: None,
    })
}

#[derive(Clone, Debug, Deserialize, ToSchema)]
pub struct DiscoveredModelRequest {
    pub upstream_model: String,
    pub display_name: String,
}

#[derive(Debug, Deserialize, ToSchema)]
pub struct DiscoverModelsRequest {
    /// Omit or pass an empty array to query the upstream model-list API.
    /// Manual identifiers are a fallback for upstreams without a list API.
    /// All discovered models start disabled and without capability claims until
    /// the explicit review operation is completed.
    #[serde(default)]
    pub models: Vec<DiscoveredModelRequest>,
}

#[utoipa::path(
    post,
    path = "/api/v1/providers/{provider_id}/discovery",
    tag = "providers",
    params(("provider_id" = Uuid, Path), ("If-Match" = String, Header)),
    request_body = DiscoverModelsRequest,
    responses((status = 200, body = ProviderCatalogResponse), (status = 412, body = Problem), (status = 422, body = Problem))
)]
async fn discover_provider_models(
    State(state): State<ApiState>,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
    payload: Result<Json<DiscoverModelsRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageProviders)?;
    let request = json(payload)?;
    let models: Vec<DiscoveredModelInput> = if request.models.is_empty() {
        catalog_connector(&state, provider_id)
            .await?
            .discover_models()
            .await
            .map_err(|detail| validation("provider", &detail))?
            .into_iter()
            .map(|model| DiscoveredModelInput {
                upstream_model: model.id,
                display_name: model.display_name,
                enabled: false,
                capabilities: Vec::new(),
            })
            .collect()
    } else {
        request
            .models
            .into_iter()
            .map(|model| DiscoveredModelInput {
                upstream_model: model.upstream_model,
                display_name: model.display_name,
                enabled: false,
                capabilities: Vec::new(),
            })
            .collect()
    };
    let store = require_store(&state)?;
    let etag = store
        .discover_provider_models(provider_id, if_match(&headers)?, &models, principal.user_id)
        .await
        .map_err(map_catalog)?;
    let provider: ProviderCatalogResponse = store
        .get_provider_catalog(provider_id)
        .await
        .map_err(map_catalog)?
        .into();
    with_etag(Json(provider), etag)
}

#[derive(Debug, Deserialize, ToSchema)]
pub struct SetModelRequest {
    pub enabled: bool,
    /// Explicit operator-reviewed capability tuples. Their provenance is
    /// recorded as `declared`; certification/probe jobs may promote provenance
    /// separately and cannot be forged by the browser.
    #[serde(default)]
    pub capabilities: Vec<CapabilityInput>,
}

#[utoipa::path(
    patch,
    path = "/api/v1/providers/{provider_id}/models/{model_id}",
    tag = "providers",
    params(("provider_id" = Uuid, Path), ("model_id" = Uuid, Path), ("If-Match" = String, Header)),
    request_body = SetModelRequest,
    responses((status = 200, body = ProviderCatalogResponse), (status = 412, body = Problem))
)]
async fn set_provider_model(
    State(state): State<ApiState>,
    Path((provider_id, model_id)): Path<(Uuid, Uuid)>,
    headers: HeaderMap,
    payload: Result<Json<SetModelRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageProviders)?;
    let request = json(payload)?;
    let store = require_store(&state)?;
    let etag = store
        .set_provider_model_enabled(
            provider_id,
            model_id,
            request.enabled,
            &request
                .capabilities
                .into_iter()
                .map(capability_record)
                .collect::<Result<Vec<_>, _>>()?,
            if_match(&headers)?,
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

#[derive(Clone, Debug, Serialize, ToSchema)]
pub struct CapabilityCertificationItemResponse {
    pub operation: String,
    pub surface: String,
    pub mode: String,
    pub succeeded: bool,
    pub error_code: Option<String>,
    pub detail: String,
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub struct CapabilityCertificationResponse {
    pub provider_id: Uuid,
    pub model_id: Uuid,
    pub status: String,
    pub checked_at: DateTime<Utc>,
    pub certified_count: usize,
    pub attempted_count: usize,
    pub results: Vec<CapabilityCertificationItemResponse>,
}

#[utoipa::path(
    post,
    path = "/api/v1/providers/{provider_id}/models/{model_id}/certify",
    tag = "providers",
    params(
        ("provider_id" = Uuid, Path),
        ("model_id" = Uuid, Path),
        ("If-Match" = String, Header, description = "Current provider ETag")
    ),
    responses(
        (status = 200, description = "Provider/model capability certification completed", body = CapabilityCertificationResponse),
        (status = 409, description = "Provider is active", body = Problem),
        (status = 412, description = "Provider or reviewed capabilities changed", body = Problem),
        (status = 422, description = "Provider or capability set cannot be certified", body = Problem)
    )
)]
async fn certify_provider_model(
    State(state): State<ApiState>,
    Path((provider_id, model_id)): Path<(Uuid, Uuid)>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageProviders)?;
    let expected_etag = if_match(&headers)?;
    let store = require_store(&state)?;
    let provider = store
        .get_provider_catalog(provider_id)
        .await
        .map_err(map_catalog)?;
    if provider.etag != expected_etag {
        return Err(map_catalog(CatalogError::PreconditionFailed));
    }
    if provider.state != olp_domain::ProviderState::Draft {
        return Err(map_catalog(CatalogError::InUse));
    }
    let model = store
        .get_provider_model_catalog(provider_id, model_id)
        .await
        .map_err(map_catalog)?;
    if model.capabilities.is_empty() || model.capabilities.len() > 16 {
        return Err(validation(
            "capabilities",
            "Review between 1 and 16 capability tuples before certification.",
        ));
    }
    let upstream_model = model.upstream_model;
    let connector = catalog_connector(&state, provider_id).await?;
    let results = stream::iter(model.capabilities.into_iter().map(|capability| {
        let connector = &connector;
        let upstream_model = &upstream_model;
        async move {
            let tuple = compatible_capability(&capability)?;
            let result = connector.certify_capability(upstream_model, tuple).await;
            Ok::<_, Problem>(certification_item(capability, result))
        }
    }))
    .buffered(4)
    .collect::<Vec<_>>()
    .await
    .into_iter()
    .collect::<Result<Vec<_>, _>>()?;

    let outcomes = results
        .iter()
        .map(|result| {
            Ok::<_, Problem>(CapabilityCertificationOutcome {
                operation: result.operation.parse().map_err(|_| Problem::internal())?,
                surface: result.surface.parse().map_err(|_| Problem::internal())?,
                mode: result.mode.parse().map_err(|_| Problem::internal())?,
                succeeded: result.succeeded,
            })
        })
        .collect::<Result<Vec<_>, _>>()?;
    let applied = store
        .apply_compatible_capability_certification(
            provider_id,
            model_id,
            expected_etag,
            principal.user_id,
            &outcomes,
        )
        .await
        .map_err(map_catalog)?;
    let status = if applied.certified_count == applied.attempted_count {
        "succeeded"
    } else if applied.certified_count == 0 {
        "failed"
    } else {
        "partial"
    };
    with_etag(
        Json(CapabilityCertificationResponse {
            provider_id,
            model_id,
            status: status.to_owned(),
            checked_at: applied.certified_at,
            certified_count: applied.certified_count,
            attempted_count: applied.attempted_count,
            results,
        }),
        applied.etag,
    )
}

fn compatible_capability(capability: &CapabilityRecord) -> Result<CompatibleCapability, Problem> {
    Ok(CompatibleCapability {
        operation: capability.operation,
        surface: capability.surface,
        mode: capability.mode,
    })
}

fn certification_item(
    capability: CapabilityRecord,
    result: Result<CapabilityCertificationEvidence, CompatibleCapabilityCertificationError>,
) -> CapabilityCertificationItemResponse {
    let (succeeded, error_code, detail) = match result {
        Ok(CapabilityCertificationEvidence::LiveProbe) => (
            true,
            None,
            "The endpoint completed the bounded request and passed the production response codec."
                .to_owned(),
        ),
        Ok(CapabilityCertificationEvidence::NativeOpenAiModelDiscoveryAndConnectorContract) => (
            true,
            None,
            "The official OpenAI endpoint returned the exact provider model from credentialed bounded discovery, and this tuple is in the closed native connector contract."
                .to_owned(),
        ),
        Err(CompatibleCapabilityCertificationError::Unsupported) => (
            false,
            Some("unsafe_or_unsupported_probe".to_owned()),
            "This tuple has no safe bounded live probe and was not certified.".to_owned(),
        ),
        Err(CompatibleCapabilityCertificationError::Transport { phase, class }) => (
            false,
            Some(transport_failure_code(class).to_owned()),
            format!("The live endpoint probe failed during {phase:?}."),
        ),
        Err(CompatibleCapabilityCertificationError::InvalidResult) => (
            false,
            Some("invalid_probe_result".to_owned()),
            "The live endpoint response did not prove the requested capability.".to_owned(),
        ),
        Err(CompatibleCapabilityCertificationError::ModelNotDiscovered) => (
            false,
            Some("model_not_discovered".to_owned()),
            "Credentialed model discovery did not return the exact reviewed provider model."
                .to_owned(),
        ),
    };
    CapabilityCertificationItemResponse {
        operation: capability.operation.to_string(),
        surface: capability.surface.to_string(),
        mode: capability.mode.to_string(),
        succeeded,
        error_code,
        detail,
    }
}

const fn transport_failure_code(class: olp_domain::AttemptFailureClass) -> &'static str {
    match class {
        olp_domain::AttemptFailureClass::Connect => "connect_failed",
        olp_domain::AttemptFailureClass::Timeout => "timeout",
        olp_domain::AttemptFailureClass::RateLimit => "rate_limited",
        olp_domain::AttemptFailureClass::UpstreamServer => "upstream_server_error",
        olp_domain::AttemptFailureClass::UpstreamClient => "upstream_rejected_probe",
        olp_domain::AttemptFailureClass::Protocol => "protocol_mismatch",
        olp_domain::AttemptFailureClass::Cancelled => "cancelled",
        olp_domain::AttemptFailureClass::Ambiguous => "ambiguous_result",
    }
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub struct RouteTargetCatalogResponse {
    pub id: Uuid,
    pub provider_model_id: Uuid,
    pub provider_id: Uuid,
    pub provider_name: String,
    pub provider_model: String,
    pub priority: i32,
    pub weight: i32,
    pub timeout_ms: i32,
    pub position: i32,
}

impl From<RouteTargetRecord> for RouteTargetCatalogResponse {
    fn from(value: RouteTargetRecord) -> Self {
        Self {
            id: value.id,
            provider_model_id: value.provider_model_id,
            provider_id: value.provider_id,
            provider_name: value.provider_name,
            provider_model: value.provider_model,
            priority: value.priority,
            weight: value.weight,
            timeout_ms: value.timeout_ms,
            position: value.position,
        }
    }
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub struct RouteDraftCatalogResponse {
    pub id: Uuid,
    pub slug: String,
    pub state: String,
    pub overall_timeout_ms: i32,
    pub max_attempts: i16,
    pub etag: Uuid,
    pub based_on_revision_id: Option<Uuid>,
    pub operations: Vec<String>,
    pub targets: Vec<RouteTargetCatalogResponse>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

impl From<RouteDraftCatalogRecord> for RouteDraftCatalogResponse {
    fn from(value: RouteDraftCatalogRecord) -> Self {
        Self {
            id: value.id,
            slug: value.slug,
            state: value.state.to_string(),
            overall_timeout_ms: value.overall_timeout_ms,
            max_attempts: value.max_attempts,
            etag: value.etag,
            based_on_revision_id: value.based_on_revision_id,
            operations: value
                .operations
                .into_iter()
                .map(|operation| operation.to_string())
                .collect(),
            targets: value.targets.into_iter().map(Into::into).collect(),
            created_at: value.created_at,
            updated_at: value.updated_at,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub struct RouteDraftListResponse {
    pub items: Vec<RouteDraftCatalogResponse>,
    pub next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/route-drafts",
    tag = "routes",
    params(("cursor" = Option<String>, Query), ("limit" = Option<u16>, Query)),
    responses((status = 200, body = RouteDraftListResponse))
)]
async fn list_route_drafts(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<RouteDraftListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let page = require_store(&state)?
        .list_route_draft_catalog(cursor, limit)
        .await
        .map_err(map_catalog)?;
    Ok(Json(RouteDraftListResponse {
        items: page.items.into_iter().map(Into::into).collect(),
        next_cursor: page.next_cursor.map(|value| value.to_string()),
    }))
}

#[utoipa::path(
    get,
    path = "/api/v1/route-drafts/{draft_id}",
    tag = "routes",
    params(("draft_id" = Uuid, Path)),
    responses((status = 200, body = RouteDraftCatalogResponse), (status = 404, body = Problem))
)]
async fn get_route_draft(
    State(state): State<ApiState>,
    Path(draft_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let draft: RouteDraftCatalogResponse = require_store(&state)?
        .get_route_draft_catalog(draft_id)
        .await
        .map_err(map_catalog)?
        .into();
    let etag = draft.etag;
    with_etag(Json(draft), etag)
}

#[derive(Debug, Deserialize, ToSchema)]
pub struct ReplaceRouteTargetRequest {
    pub provider_model_id: Uuid,
    pub priority: i32,
    pub weight: i32,
    pub timeout_ms: i32,
}

#[derive(Debug, Deserialize, ToSchema)]
pub struct ReplaceRouteDraftRequest {
    pub slug: String,
    pub operations: Vec<String>,
    pub overall_timeout_ms: i32,
    pub max_attempts: i16,
    pub targets: Vec<ReplaceRouteTargetRequest>,
}

#[utoipa::path(
    put,
    path = "/api/v1/route-drafts/{draft_id}",
    tag = "routes",
    params(("draft_id" = Uuid, Path), ("If-Match" = String, Header)),
    request_body = ReplaceRouteDraftRequest,
    responses((status = 200, body = RouteDraftCatalogResponse), (status = 412, body = Problem), (status = 422, body = Problem))
)]
async fn replace_route_draft(
    State(state): State<ApiState>,
    Path(draft_id): Path<Uuid>,
    headers: HeaderMap,
    payload: Result<Json<ReplaceRouteDraftRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageRoutes)?;
    let request = json(payload)?;
    let targets: Vec<_> = request
        .targets
        .into_iter()
        .map(|target| {
            (
                target.provider_model_id,
                target.priority,
                target.weight,
                target.timeout_ms,
            )
        })
        .collect();
    let store = require_store(&state)?;
    let etag = store
        .replace_route_draft_catalog(
            draft_id,
            if_match(&headers)?,
            &ReplaceRouteDraftCatalogInput {
                slug: request.slug,
                operations: request
                    .operations
                    .into_iter()
                    .map(|operation| {
                        operation
                            .parse()
                            .map_err(|_| validation("operations", "A route operation is invalid."))
                    })
                    .collect::<Result<Vec<_>, _>>()?,
                overall_timeout_ms: request.overall_timeout_ms,
                max_attempts: request.max_attempts,
                targets,
            },
            principal.user_id,
        )
        .await
        .map_err(map_catalog)?;
    let draft: RouteDraftCatalogResponse = store
        .get_route_draft_catalog(draft_id)
        .await
        .map_err(map_catalog)?
        .into();
    with_etag(Json(draft), etag)
}

#[utoipa::path(
    delete,
    path = "/api/v1/route-drafts/{draft_id}",
    tag = "routes",
    params(("draft_id" = Uuid, Path), ("If-Match" = String, Header)),
    responses((status = 204), (status = 409, body = Problem), (status = 412, body = Problem))
)]
async fn delete_route_draft(
    State(state): State<ApiState>,
    Path(draft_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageRoutes)?;
    let expected_etag = if_match(&headers)?;
    require_store(&state)?
        .delete_route_draft_catalog(draft_id, expected_etag, principal.user_id)
        .await
        .map_err(map_catalog)?;
    with_etag(StatusCode::NO_CONTENT, expected_etag)
}

#[derive(Debug, Deserialize, ToSchema)]
pub struct SimulateRouteRequest {
    pub operation: String,
    pub surface: String,
    pub mode: String,
    pub seed: String,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct RouteSimulationTargetResponse {
    pub target_id: Uuid,
    pub provider_id: Uuid,
    pub provider_name: String,
    pub provider_model: String,
    pub priority: i32,
    pub eligible: bool,
    pub reason: Option<String>,
    pub attempt: Option<usize>,
}

impl From<RouteSimulationTarget> for RouteSimulationTargetResponse {
    fn from(value: RouteSimulationTarget) -> Self {
        Self {
            target_id: value.target_id,
            provider_id: value.provider_id,
            provider_name: value.provider_name,
            provider_model: value.provider_model,
            priority: value.priority,
            eligible: value.eligible,
            reason: value.reason,
            attempt: value.attempt,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub struct RouteSimulationResponse {
    pub deterministic_seed: String,
    pub operation: String,
    pub surface: String,
    pub mode: String,
    pub targets: Vec<RouteSimulationTargetResponse>,
}

impl From<RouteSimulation> for RouteSimulationResponse {
    fn from(value: RouteSimulation) -> Self {
        Self {
            deterministic_seed: value.deterministic_seed,
            operation: value.operation.to_string(),
            surface: value.surface.to_string(),
            mode: value.mode.to_string(),
            targets: value.targets.into_iter().map(Into::into).collect(),
        }
    }
}

#[utoipa::path(
    post,
    path = "/api/v1/route-drafts/{draft_id}/simulate",
    tag = "routes",
    params(("draft_id" = Uuid, Path)),
    request_body = SimulateRouteRequest,
    responses((status = 200, body = RouteSimulationResponse), (status = 422, body = Problem))
)]
async fn simulate_route_draft(
    State(state): State<ApiState>,
    Path(draft_id): Path<Uuid>,
    headers: HeaderMap,
    payload: Result<Json<SimulateRouteRequest>, JsonRejection>,
) -> Result<Json<RouteSimulationResponse>, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageRoutes)?;
    let request = json(payload)?;
    let simulation = require_store(&state)?
        .simulate_route_draft_catalog(
            draft_id,
            request
                .operation
                .parse()
                .map_err(|_| validation("operation", "The operation is invalid."))?,
            request
                .surface
                .parse()
                .map_err(|_| validation("surface", "The surface is invalid."))?,
            request
                .mode
                .parse()
                .map_err(|_| validation("mode", "The transport mode is invalid."))?,
            &request.seed,
        )
        .await
        .map_err(map_catalog)?;
    Ok(Json(simulation.into()))
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub struct RouteRevisionResponse {
    pub id: Uuid,
    pub route_id: Uuid,
    pub revision: i32,
    pub slug: String,
    pub overall_timeout_ms: i32,
    pub max_attempts: i16,
    pub source_draft_id: Uuid,
    pub activated_by: Uuid,
    pub activated_at: DateTime<Utc>,
    pub operations: Vec<String>,
    pub targets: Vec<RouteTargetCatalogResponse>,
}

impl From<RouteRevisionCatalogRecord> for RouteRevisionResponse {
    fn from(value: RouteRevisionCatalogRecord) -> Self {
        Self {
            id: value.id,
            route_id: value.route_id,
            revision: value.revision,
            slug: value.slug,
            overall_timeout_ms: value.overall_timeout_ms,
            max_attempts: value.max_attempts,
            source_draft_id: value.source_draft_id,
            activated_by: value.activated_by,
            activated_at: value.activated_at,
            operations: value
                .operations
                .into_iter()
                .map(|operation| operation.to_string())
                .collect(),
            targets: value.targets.into_iter().map(Into::into).collect(),
        }
    }
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub struct RouteCatalogResponse {
    pub id: Uuid,
    pub slug: String,
    pub created_at: DateTime<Utc>,
    pub revision_count: u64,
    pub latest_revision: RouteRevisionResponse,
}

impl From<RouteCatalogRecord> for RouteCatalogResponse {
    fn from(value: RouteCatalogRecord) -> Self {
        Self {
            id: value.id,
            slug: value.slug,
            created_at: value.created_at,
            revision_count: value.revision_count,
            latest_revision: value.latest_revision.into(),
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub struct RouteListResponse {
    pub items: Vec<RouteCatalogResponse>,
    pub next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/routes",
    tag = "routes",
    params(
        ("cursor" = Option<String>, Query),
        ("limit" = Option<u16>, Query, minimum = 1, maximum = 100)
    ),
    responses((status = 200, body = RouteListResponse), (status = 401, body = Problem), (status = 403, body = Problem))
)]
async fn list_routes(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<RouteListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let routes = require_store(&state)?
        .list_routes_catalog(cursor, limit)
        .await
        .map_err(map_catalog)?;
    Ok(Json(RouteListResponse {
        items: routes.items.into_iter().map(Into::into).collect(),
        next_cursor: routes.next_cursor.map(|value| value.to_string()),
    }))
}

#[utoipa::path(
    get,
    path = "/api/v1/routes/{route_id}",
    tag = "routes",
    params(("route_id" = Uuid, Path)),
    responses((status = 200, body = RouteCatalogResponse), (status = 404, body = Problem))
)]
async fn get_route(
    State(state): State<ApiState>,
    Path(route_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Json<RouteCatalogResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let route = require_store(&state)?
        .get_route_catalog(route_id)
        .await
        .map_err(map_catalog)?;
    Ok(Json(route.into()))
}

#[derive(Debug, Serialize, ToSchema)]
pub struct RouteRevisionListResponse {
    pub items: Vec<RouteRevisionResponse>,
    pub next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/routes/{route_id}/revisions",
    tag = "routes",
    params(
        ("route_id" = Uuid, Path),
        ("cursor" = Option<String>, Query),
        ("limit" = Option<u16>, Query, minimum = 1, maximum = 100)
    ),
    responses((status = 200, body = RouteRevisionListResponse), (status = 404, body = Problem))
)]
async fn list_route_revisions(
    State(state): State<ApiState>,
    Path(route_id): Path<Uuid>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<RouteRevisionListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let page = require_store(&state)?
        .list_route_revisions_catalog(route_id, cursor, limit)
        .await
        .map_err(map_catalog)?;
    let items = page.items.into_iter().map(Into::into).collect();
    Ok(Json(RouteRevisionListResponse {
        items,
        next_cursor: page.next_cursor.map(|cursor| cursor.to_string()),
    }))
}

#[utoipa::path(
    get,
    path = "/api/v1/routes/{route_id}/revisions/{revision_id}",
    tag = "routes",
    params(("route_id" = Uuid, Path), ("revision_id" = Uuid, Path)),
    responses((status = 200, body = RouteRevisionResponse), (status = 404, body = Problem))
)]
async fn get_route_revision(
    State(state): State<ApiState>,
    Path((route_id, revision_id)): Path<(Uuid, Uuid)>,
    headers: HeaderMap,
) -> Result<Json<RouteRevisionResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    Ok(Json(
        require_store(&state)?
            .get_route_revision_catalog(route_id, revision_id)
            .await
            .map_err(map_catalog)?
            .into(),
    ))
}

#[derive(Debug, Deserialize)]
struct DiffQuery {
    from: Uuid,
    to: Uuid,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct RouteRevisionDiffResponse {
    pub from_revision: i32,
    pub to_revision: i32,
    pub slug_changed: bool,
    pub timeout_changed: bool,
    pub max_attempts_changed: bool,
    pub operations_added: Vec<String>,
    pub operations_removed: Vec<String>,
    pub targets_added: Vec<String>,
    pub targets_removed: Vec<String>,
    pub targets_changed: Vec<String>,
}

impl From<RouteRevisionDiff> for RouteRevisionDiffResponse {
    fn from(value: RouteRevisionDiff) -> Self {
        Self {
            from_revision: value.from_revision,
            to_revision: value.to_revision,
            slug_changed: value.slug_changed,
            timeout_changed: value.timeout_changed,
            max_attempts_changed: value.max_attempts_changed,
            operations_added: value
                .operations_added
                .into_iter()
                .map(|operation| operation.to_string())
                .collect(),
            operations_removed: value
                .operations_removed
                .into_iter()
                .map(|operation| operation.to_string())
                .collect(),
            targets_added: value.targets_added,
            targets_removed: value.targets_removed,
            targets_changed: value.targets_changed,
        }
    }
}

#[utoipa::path(
    get,
    path = "/api/v1/routes/{route_id}/revisions/diff",
    tag = "routes",
    params(("route_id" = Uuid, Path), ("from" = Uuid, Query), ("to" = Uuid, Query)),
    responses((status = 200, body = RouteRevisionDiffResponse), (status = 404, body = Problem))
)]
async fn diff_route_revisions(
    State(state): State<ApiState>,
    Path(route_id): Path<Uuid>,
    headers: HeaderMap,
    Query(query): Query<DiffQuery>,
) -> Result<Json<RouteRevisionDiffResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    Ok(Json(
        require_store(&state)?
            .diff_route_revisions_catalog(route_id, query.from, query.to)
            .await
            .map_err(map_catalog)?
            .into(),
    ))
}

#[utoipa::path(
    post,
    path = "/api/v1/routes/{route_id}/revisions/{revision_id}/restore-as-draft",
    tag = "routes",
    params(("route_id" = Uuid, Path), ("revision_id" = Uuid, Path), ("Idempotency-Key" = String, Header)),
    responses((status = 201, body = RouteDraftCatalogResponse), (status = 409, body = Problem))
)]
async fn restore_route_revision(
    State(state): State<ApiState>,
    Path((route_id, revision_id)): Path<(Uuid, Uuid)>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageRoutes)?;
    let draft: RouteDraftCatalogResponse = require_store(&state)?
        .restore_route_revision_as_draft(
            route_id,
            revision_id,
            principal.user_id,
            require_idempotency_key(&headers)?,
        )
        .await
        .map_err(map_catalog)?
        .into();
    with_etag((StatusCode::CREATED, Json(draft.clone())), draft.etag)
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub struct ApiKeyCatalogResponse {
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
pub struct ApiKeyListResponse {
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
async fn list_api_keys(
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
async fn get_api_key(
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
pub struct UpdateApiKeyRequest {
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
pub struct ApiKeyMutationResponse {
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
async fn update_api_key(
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
pub struct RotateApiKeyResponse {
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
async fn rotate_api_key(
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

fn with_etag(response: impl IntoResponse, etag: Uuid) -> Result<Response, Problem> {
    let mut response = response.into_response();
    response.headers_mut().insert(
        header::ETAG,
        HeaderValue::from_str(&format!("\"{etag}\"")).map_err(|_| Problem::internal())?,
    );
    Ok(response)
}

fn json<T>(payload: Result<Json<T>, JsonRejection>) -> Result<T, Problem> {
    payload.map(|Json(value)| value).map_err(|error| {
        Problem::bad_request("invalid_json", format!("Request body is invalid: {error}"))
    })
}

fn validation(field: &str, detail: &str) -> Problem {
    let mut errors = FieldErrors::new();
    errors.insert(field.to_owned(), vec![detail.to_owned()]);
    Problem::validation(errors)
}

fn map_catalog(error: CatalogError) -> Problem {
    match error {
        CatalogError::NotFound => Problem::new(
            StatusCode::NOT_FOUND,
            "catalog_resource_not_found",
            "Resource not found",
            "The requested catalog resource does not exist.",
        ),
        CatalogError::PreconditionFailed => Problem::new(
            StatusCode::PRECONDITION_FAILED,
            "etag_mismatch",
            "Precondition failed",
            "The resource changed after it was loaded. Refresh and retry.",
        ),
        CatalogError::InUse => Problem::conflict(
            "catalog_resource_in_use",
            "The resource is active or referenced and cannot be removed.",
        ),
        CatalogError::Invalid(detail) => validation("catalog", &detail),
        CatalogError::ProviderRevisionDiffTooLarge { dimension, maximum } => validation(
            "revisions",
            &format!("provider revision diff supports at most {maximum} {dimension} per revision"),
        ),
        CatalogError::IdempotencyConflict => Problem::conflict(
            "idempotency_key_reused",
            "This Idempotency-Key has already been used for this operation.",
        ),
        CatalogError::IdempotencyInProgress => Problem::conflict(
            "idempotency_in_progress",
            "An operation with this Idempotency-Key is still in progress.",
        ),
        CatalogError::Persistence(error) => crate::management::map_persistence(error),
        CatalogError::RuntimeCompile(error) => {
            error!(%error, "runtime compilation failed after catalog mutation");
            Problem::internal()
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn write_only_secret_debug_is_redacted() {
        assert_eq!(
            format!("{:?}", WriteOnlySecret::new("top-secret".to_owned())),
            "WriteOnlySecret([REDACTED])"
        );
    }

    #[test]
    fn cursor_and_page_size_are_strict() {
        assert!(
            page(PageQuery {
                cursor: Some("bad".to_owned()),
                limit: None
            })
            .is_err()
        );
        assert!(
            page(PageQuery {
                cursor: None,
                limit: Some(0)
            })
            .is_err()
        );
        assert_eq!(
            page(PageQuery {
                cursor: None,
                limit: Some(100)
            })
            .unwrap(),
            (None, 100)
        );
    }

    #[test]
    fn compatible_certification_contract_requires_etag_and_reports_evidence() {
        let document = serde_json::to_value(CatalogApiDoc::openapi()).unwrap();
        let action =
            &document["paths"]["/api/v1/providers/{provider_id}/models/{model_id}/certify"]["post"];
        assert!(
            action["parameters"]
                .as_array()
                .unwrap()
                .iter()
                .any(|parameter| {
                    parameter["name"] == "If-Match"
                        && parameter["in"] == "header"
                        && parameter["required"] == true
                })
        );
        assert!(action["responses"].get("200").is_some());
        assert!(action["responses"].get("412").is_some());

        let item = certification_item(
            CapabilityRecord {
                operation: "image_generation".parse().unwrap(),
                surface: "open_ai".parse().unwrap(),
                mode: "unary".parse().unwrap(),
                source: olp_domain::CapabilitySource::Declared,
                certified_at: None,
            },
            Err(CompatibleCapabilityCertificationError::Unsupported),
        );
        assert!(!item.succeeded);
        assert_eq!(
            item.error_code.as_deref(),
            Some("unsafe_or_unsupported_probe")
        );

        let native = certification_item(
            CapabilityRecord {
                operation: "image_generation".parse().unwrap(),
                surface: "open_ai".parse().unwrap(),
                mode: "streaming".parse().unwrap(),
                source: olp_domain::CapabilitySource::Declared,
                certified_at: None,
            },
            Ok(CapabilityCertificationEvidence::NativeOpenAiModelDiscoveryAndConnectorContract),
        );
        assert!(native.succeeded);
        assert!(native.error_code.is_none());
        assert!(native.detail.contains("exact provider model"));
        assert!(native.detail.contains("closed native connector contract"));
    }

    #[test]
    fn provider_probe_is_connectivity_only_and_etag_bound() {
        let document = serde_json::to_value(CatalogApiDoc::openapi()).unwrap();
        let action = &document["paths"]["/api/v1/providers/{provider_id}/probe"]["post"];
        assert!(action.get("requestBody").is_none());
        assert!(
            action["parameters"]
                .as_array()
                .unwrap()
                .iter()
                .any(|parameter| {
                    parameter["name"] == "If-Match"
                        && parameter["in"] == "header"
                        && parameter["required"] == true
                })
        );
        assert!(action["responses"].get("412").is_some());
    }

    #[test]
    fn provider_revision_restore_contract_never_exposes_or_restores_credentials() {
        let document = serde_json::to_value(CatalogApiDoc::openapi()).unwrap();
        let properties =
            document["components"]["schemas"]["ProviderRevisionResponse"]["properties"]
                .as_object()
                .unwrap();
        assert!(properties.contains_key("historical_credential_version"));
        assert!(!properties.contains_key("credential_version_id"));
        assert!(!properties.contains_key("credential"));
        assert!(!properties.contains_key("secret"));

        let action = &document["paths"]["/api/v1/providers/{provider_id}/revisions/{revision_id}/restore-as-draft"]
            ["post"];
        let parameters = action["parameters"].as_array().unwrap();
        for required_header in ["If-Match", "Idempotency-Key"] {
            assert!(parameters.iter().any(|parameter| {
                parameter["name"] == required_header
                    && parameter["in"] == "header"
                    && parameter["required"] == true
            }));
        }
        assert!(action["responses"].get("412").is_some());
    }

    #[test]
    fn provider_and_revision_model_inventories_are_bounded_pages() {
        let document = serde_json::to_value(CatalogApiDoc::openapi()).unwrap();
        for schema in [
            "ProviderSummaryResponse",
            "ProviderCatalogResponse",
            "ProviderRevisionSummaryResponse",
            "ProviderRevisionResponse",
        ] {
            let properties = document["components"]["schemas"][schema]["properties"]
                .as_object()
                .unwrap();
            assert!(!properties.contains_key("models"));
            assert!(properties.contains_key("model_count"));
            assert!(properties.contains_key("enabled_model_count"));
            assert!(properties.contains_key("capability_count"));
            assert!(properties.contains_key("certified_capability_count"));
        }
        for path in [
            "/api/v1/provider-models",
            "/api/v1/providers/{provider_id}/models",
            "/api/v1/providers/{provider_id}/revisions/{revision_id}/models",
        ] {
            let action = &document["paths"][path]["get"];
            assert!(action["responses"].get("200").is_some());
            let parameters = action["parameters"].as_array().unwrap();
            assert!(
                parameters
                    .iter()
                    .any(|parameter| parameter["name"] == "cursor")
            );
            assert!(
                parameters
                    .iter()
                    .any(|parameter| parameter["name"] == "limit")
            );
        }
        let inventory_parameters =
            document["paths"]["/api/v1/provider-models"]["get"]["parameters"]
                .as_array()
                .unwrap();
        assert!(
            inventory_parameters
                .iter()
                .any(|parameter| parameter["name"] == "enabled")
        );
    }

    #[test]
    fn provider_revision_diff_contract_documents_hard_response_ceilings() {
        let document = serde_json::to_value(CatalogApiDoc::openapi()).unwrap();
        let action = &document["paths"]["/api/v1/providers/{provider_id}/revisions/diff"]["get"];
        assert!(action["responses"].get("422").is_some());

        let properties =
            &document["components"]["schemas"]["ProviderRevisionDiffResponse"]["properties"];
        for field in ["models_added", "models_removed", "models_changed"] {
            assert_eq!(properties[field]["maxItems"], 2_000);
        }
        for field in ["capabilities_added", "capabilities_removed"] {
            assert_eq!(properties[field]["maxItems"], 32_000);
        }

        let problem = map_catalog(CatalogError::ProviderRevisionDiffTooLarge {
            dimension: "models",
            maximum: 2_000,
        });
        assert_eq!(problem.status, StatusCode::UNPROCESSABLE_ENTITY.as_u16());
        assert_eq!(
            problem.errors["revisions"],
            ["provider revision diff supports at most 2000 models per revision"]
        );
    }
}
