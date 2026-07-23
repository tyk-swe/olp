use axum::{
    Json,
    extract::{Path, Query, State, rejection::JsonRejection},
    http::{HeaderMap, StatusCode},
    response::Response,
};
use chrono::{DateTime, Utc};
use olp_storage::{
    ReplaceRouteDraftInput, RouteDraftRecord, RouteRecord, RouteRevisionDiff, RouteRevisionRecord,
    RouteSimulation, RouteSimulationTarget, RouteTargetRecord,
};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;
use uuid::Uuid;

use crate::{
    ManagementState, Problem,
    management_api::{
        Permission, if_match, require_idempotency_key, require_mutation_session,
        require_permission, require_read_session,
    },
};

use crate::management_api::configuration::common::{
    DiffQuery, PageQuery, json, map_configuration_resource, page, validation, with_etag,
};

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct RouteTargetResponse {
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

impl From<RouteTargetRecord> for RouteTargetResponse {
    fn from(value: RouteTargetRecord) -> Self {
        Self {
            id: value.id,
            provider_model_id: value.provider_model_id,
            provider_id: value.provider_id,
            provider_name: value.provider_name,
            provider_model: value.upstream_model,
            priority: value.priority,
            weight: value.weight,
            timeout_ms: value.timeout_ms,
            position: value.position,
        }
    }
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct RouteDraftDetailResponse {
    pub id: Uuid,
    pub slug: String,
    pub state: String,
    pub overall_timeout_ms: i32,
    pub max_attempts: i16,
    pub etag: Uuid,
    pub based_on_revision_id: Option<Uuid>,
    pub operations: Vec<String>,
    pub targets: Vec<RouteTargetResponse>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

impl From<RouteDraftRecord> for RouteDraftDetailResponse {
    fn from(value: RouteDraftRecord) -> Self {
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
pub(crate) struct RouteDraftListResponse {
    pub items: Vec<RouteDraftDetailResponse>,
    pub next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/route-drafts",
    tag = "routes",
    params(("cursor" = Option<String>, Query), ("limit" = Option<u16>, Query)),
    responses((status = 200, body = RouteDraftListResponse))
)]
pub(crate) async fn list_route_drafts(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<RouteDraftListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let page = state
        .store()
        .list_route_drafts(cursor, limit)
        .await
        .map_err(map_configuration_resource)?;
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
    responses((status = 200, body = RouteDraftDetailResponse), (status = 404, body = Problem))
)]
pub(crate) async fn get_route_draft(
    State(state): State<ManagementState>,
    Path(draft_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let draft: RouteDraftDetailResponse = state
        .store()
        .get_route_draft(draft_id)
        .await
        .map_err(map_configuration_resource)?
        .into();
    let etag = draft.etag;
    with_etag(Json(draft), etag)
}

#[derive(Debug, Deserialize, ToSchema)]
pub(crate) struct ReplaceRouteTargetRequest {
    pub provider_model_id: Uuid,
    pub priority: i32,
    pub weight: i32,
    pub timeout_ms: i32,
}

#[derive(Debug, Deserialize, ToSchema)]
pub(crate) struct ReplaceRouteDraftRequest {
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
    responses((status = 200, body = RouteDraftDetailResponse), (status = 412, body = Problem), (status = 422, body = Problem))
)]
pub(crate) async fn replace_route_draft(
    State(state): State<ManagementState>,
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
    let store = state.store();
    let etag = store
        .replace_route_draft(
            draft_id,
            if_match(&headers)?,
            &ReplaceRouteDraftInput {
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
        .map_err(map_configuration_resource)?;
    let draft: RouteDraftDetailResponse = store
        .get_route_draft(draft_id)
        .await
        .map_err(map_configuration_resource)?
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
pub(crate) async fn delete_route_draft(
    State(state): State<ManagementState>,
    Path(draft_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageRoutes)?;
    let expected_etag = if_match(&headers)?;
    state
        .store()
        .delete_route_draft(draft_id, expected_etag, principal.user_id)
        .await
        .map_err(map_configuration_resource)?;
    with_etag(StatusCode::NO_CONTENT, expected_etag)
}

#[derive(Debug, Deserialize, ToSchema)]
pub(crate) struct SimulateRouteRequest {
    pub operation: String,
    pub surface: String,
    pub mode: String,
    pub seed: String,
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct RouteSimulationTargetResponse {
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
            provider_model: value.upstream_model,
            priority: value.priority,
            eligible: value.eligible,
            reason: value.reason,
            attempt: value.attempt,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct RouteSimulationResponse {
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
pub(crate) async fn simulate_route_draft(
    State(state): State<ManagementState>,
    Path(draft_id): Path<Uuid>,
    headers: HeaderMap,
    payload: Result<Json<SimulateRouteRequest>, JsonRejection>,
) -> Result<Json<RouteSimulationResponse>, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageRoutes)?;
    let request = json(payload)?;
    let simulation = state
        .store()
        .simulate_route_draft(
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
        .map_err(map_configuration_resource)?;
    Ok(Json(simulation.into()))
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct RouteRevisionResponse {
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
    pub targets: Vec<RouteTargetResponse>,
}

impl From<RouteRevisionRecord> for RouteRevisionResponse {
    fn from(value: RouteRevisionRecord) -> Self {
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
pub(crate) struct RouteDetailResponse {
    pub id: Uuid,
    pub slug: String,
    pub created_at: DateTime<Utc>,
    pub revision_count: u64,
    pub latest_revision: RouteRevisionResponse,
}

impl From<RouteRecord> for RouteDetailResponse {
    fn from(value: RouteRecord) -> Self {
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
pub(crate) struct RouteListResponse {
    pub items: Vec<RouteDetailResponse>,
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
pub(crate) async fn list_routes(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<RouteListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let routes = state
        .store()
        .list_routes(cursor, limit)
        .await
        .map_err(map_configuration_resource)?;
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
    responses((status = 200, body = RouteDetailResponse), (status = 404, body = Problem))
)]
pub(crate) async fn get_route(
    State(state): State<ManagementState>,
    Path(route_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Json<RouteDetailResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let route = state
        .store()
        .get_route(route_id)
        .await
        .map_err(map_configuration_resource)?;
    Ok(Json(route.into()))
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct RouteRevisionListResponse {
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
pub(crate) async fn list_route_revisions(
    State(state): State<ManagementState>,
    Path(route_id): Path<Uuid>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<RouteRevisionListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let page = state
        .store()
        .list_route_revisions(route_id, cursor, limit)
        .await
        .map_err(map_configuration_resource)?;
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
pub(crate) async fn get_route_revision(
    State(state): State<ManagementState>,
    Path((route_id, revision_id)): Path<(Uuid, Uuid)>,
    headers: HeaderMap,
) -> Result<Json<RouteRevisionResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    Ok(Json(
        state
            .store()
            .get_route_revision(route_id, revision_id)
            .await
            .map_err(map_configuration_resource)?
            .into(),
    ))
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct RouteRevisionDiffResponse {
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
pub(crate) async fn diff_route_revisions(
    State(state): State<ManagementState>,
    Path(route_id): Path<Uuid>,
    headers: HeaderMap,
    Query(query): Query<DiffQuery>,
) -> Result<Json<RouteRevisionDiffResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    Ok(Json(
        state
            .store()
            .diff_route_revisions(route_id, query.from, query.to)
            .await
            .map_err(map_configuration_resource)?
            .into(),
    ))
}

#[utoipa::path(
    post,
    path = "/api/v1/routes/{route_id}/revisions/{revision_id}/restore-as-draft",
    tag = "routes",
    params(("route_id" = Uuid, Path), ("revision_id" = Uuid, Path), ("Idempotency-Key" = String, Header)),
    responses((status = 201, body = RouteDraftDetailResponse), (status = 409, body = Problem))
)]
pub(crate) async fn restore_route_revision(
    State(state): State<ManagementState>,
    Path((route_id, revision_id)): Path<(Uuid, Uuid)>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageRoutes)?;
    let draft: RouteDraftDetailResponse = state
        .store()
        .restore_route_revision_as_draft(
            route_id,
            revision_id,
            principal.user_id,
            require_idempotency_key(&headers)?,
        )
        .await
        .map_err(map_configuration_resource)?
        .into();
    with_etag((StatusCode::CREATED, Json(draft.clone())), draft.etag)
}
