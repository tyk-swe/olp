use crate::access::policy::Permission;
use crate::database::idempotency::operations;
use crate::protocols::canonical::identity::OperationKind;
use crate::routes::records::ReplaceRouteDraftInput;
use crate::routes::records::RouteDraftRecord;
use crate::routes::records::RouteRecord;
use crate::routes::records::RouteRevisionDiff;
use crate::routes::records::RouteRevisionRecord;
use crate::routes::records::RouteSimulation;
use crate::routes::records::RouteSimulationTarget;
use crate::routes::records::RouteTargetRecord;
use axum::Json;
use axum::extract::Path;
use axum::extract::Query;
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
use crate::access::principal::MutationPrincipal;
use crate::access::principal::ReadPrincipal;
use crate::http::control::error_mapping::map_configuration;
use crate::http::control::idempotency::MutationReply;
use crate::http::control::idempotency::ReplayableMutation;
use crate::http::control::json_payload::json_payload;
use crate::http::control::pagination::DiffQuery;
use crate::http::control::pagination::PageQuery;
use crate::http::control::pagination::page;
use crate::http::control::preconditions::if_match;
use crate::http::control::preconditions::with_etag;
use crate::http::control::provenance::Provenance;
use crate::http::control::state::ManagementState;
use crate::http::problem::Problem;

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct RouteTargetResponse {
    pub id: Uuid,
    pub provider_model_id: Uuid,
    pub provider_id: Uuid,
    pub provider_name: String,
    pub provider_model: String,
    /// False when the provider was disabled or the model left the provider's
    /// activated revision. The target is still part of the stored route.
    pub available: bool,
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
            available: value.available,
            priority: value.priority,
            weight: value.weight,
            timeout_ms: value.timeout_ms,
            position: value.position,
        }
    }
}

fn operation_names(operations: Vec<OperationKind>) -> Vec<String> {
    operations
        .into_iter()
        .map(|operation| operation.to_string())
        .collect()
}

fn target_responses(targets: Vec<RouteTargetRecord>) -> Vec<RouteTargetResponse> {
    targets.into_iter().map(Into::into).collect()
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
    /// Email of the operator who created the draft.
    pub created_by_email: Option<String>,
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
            operations: operation_names(value.operations),
            targets: target_responses(value.targets),
            created_at: value.created_at,
            updated_at: value.updated_at,
            created_by_email: value.created_by_email,
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
    path = "/api/v3/route-drafts",
    tag = "routes",
    params(PageQuery),
    responses(
        (status = 200, body = RouteDraftListResponse),
        (status = 400, description = "Malformed query parameters, or an invalid cursor or page size", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [])))]
pub(crate) async fn list_route_drafts(
    State(state): State<ManagementState>,
    Query(query): Query<PageQuery>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<RouteDraftListResponse>, Problem> {
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let page =
        crate::routes::repository::list_route_drafts(&state.request_boundary.pool, cursor, limit)
            .await
            .map_err(map_configuration)?;
    Ok(Json(RouteDraftListResponse {
        items: page.items.into_iter().map(Into::into).collect(),
        next_cursor: page.next_cursor.map(|value| value.to_string()),
    }))
}

#[utoipa::path(
    get,
    path = "/api/v3/route-drafts/{draft_id}",
    tag = "routes",
    params(("draft_id" = Uuid, Path)),
    responses((status = 200, body = RouteDraftDetailResponse), (status = 404, body = Problem, content_type = "application/problem+json"))
, security(("sessionCookie" = [])))]
pub(crate) async fn get_route_draft(
    State(state): State<ManagementState>,
    Path(draft_id): Path<Uuid>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ReadConfiguration)?;
    let draft: RouteDraftDetailResponse =
        crate::routes::repository::get_route_draft(&state.request_boundary.pool, draft_id)
            .await
            .map_err(map_configuration)?
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
    path = "/api/v3/route-drafts/{draft_id}",
    tag = "routes",
    params(("draft_id" = Uuid, Path), ("If-Match" = String, Header)),
    request_body = ReplaceRouteDraftRequest,
    responses((status = 200, body = RouteDraftDetailResponse), (status = 412, body = Problem, content_type = "application/problem+json"), (status = 422, body = Problem, content_type = "application/problem+json"))
, security(("sessionCookie" = [], "csrfToken" = [])))]
pub(crate) async fn replace_route_draft(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    Path(draft_id): Path<Uuid>,
    headers: HeaderMap,
    MutationPrincipal(principal): MutationPrincipal,
    payload: Result<Json<ReplaceRouteDraftRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ManageRoutes)?;
    let request = json_payload(payload)?;
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
    let pool = &state.request_boundary.pool;
    let etag = crate::routes::repository::replace_route_draft(
        pool,
        &provenance,
        draft_id,
        if_match(&headers)?,
        &ReplaceRouteDraftInput {
            slug: request.slug,
            operations: request
                .operations
                .into_iter()
                .map(|operation| {
                    operation.parse().map_err(|_| {
                        Problem::field_validation("operations", "A route operation is invalid.")
                    })
                })
                .collect::<Result<Vec<_>, _>>()?,
            overall_timeout_ms: request.overall_timeout_ms,
            max_attempts: request.max_attempts,
            targets,
        },
        principal.user_id,
    )
    .await
    .map_err(map_configuration)?;
    let draft: RouteDraftDetailResponse =
        crate::routes::repository::get_route_draft(pool, draft_id)
            .await
            .map_err(map_configuration)?
            .into();
    with_etag(Json(draft), etag)
}

#[utoipa::path(
    delete,
    path = "/api/v3/route-drafts/{draft_id}",
    tag = "routes",
    params(("draft_id" = Uuid, Path), ("If-Match" = String, Header)),
    responses((status = 204), (status = 409, body = Problem, content_type = "application/problem+json"), (status = 412, body = Problem, content_type = "application/problem+json"))
, security(("sessionCookie" = [], "csrfToken" = [])))]
pub(crate) async fn delete_route_draft(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    Path(draft_id): Path<Uuid>,
    headers: HeaderMap,
    MutationPrincipal(principal): MutationPrincipal,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ManageRoutes)?;
    let expected_etag = if_match(&headers)?;
    crate::routes::repository::delete_route_draft(
        &state.request_boundary.pool,
        &provenance,
        draft_id,
        expected_etag,
        principal.user_id,
    )
    .await
    .map_err(map_configuration)?;
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
    path = "/api/v3/route-drafts/{draft_id}/simulate",
    tag = "routes",
    params(("draft_id" = Uuid, Path)),
    request_body = SimulateRouteRequest,
    responses((status = 200, body = RouteSimulationResponse), (status = 422, body = Problem, content_type = "application/problem+json"))
, security(("sessionCookie" = [], "csrfToken" = [])))]
pub(crate) async fn simulate_route_draft(
    State(state): State<ManagementState>,
    Path(draft_id): Path<Uuid>,
    MutationPrincipal(principal): MutationPrincipal,
    payload: Result<Json<SimulateRouteRequest>, JsonRejection>,
) -> Result<Json<RouteSimulationResponse>, Problem> {
    require_permission(&principal, Permission::ManageRoutes)?;
    let request = json_payload(payload)?;
    let simulation = crate::routes::repository::simulate_route_draft(
        &state.request_boundary.pool,
        draft_id,
        request
            .operation
            .parse()
            .map_err(|_| Problem::field_validation("operation", "The operation is invalid."))?,
        request
            .surface
            .parse()
            .map_err(|_| Problem::field_validation("surface", "The surface is invalid."))?,
        request
            .mode
            .parse()
            .map_err(|_| Problem::field_validation("mode", "The transport mode is invalid."))?,
        &request.seed,
    )
    .await
    .map_err(map_configuration)?;
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
            operations: operation_names(value.operations),
            targets: target_responses(value.targets),
        }
    }
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct RouteDetailResponse {
    pub id: Uuid,
    pub slug: String,
    pub created_at: DateTime<Utc>,
    /// Email of the operator who created the route.
    pub created_by_email: Option<String>,
    pub revision_count: u64,
    pub latest_revision: RouteRevisionResponse,
}

impl From<RouteRecord> for RouteDetailResponse {
    fn from(value: RouteRecord) -> Self {
        Self {
            id: value.id,
            slug: value.slug,
            created_at: value.created_at,
            created_by_email: value.created_by_email,
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
    path = "/api/v3/routes",
    tag = "routes",
    params(
        PageQuery,
    ),
    responses(
        (status = 200, body = RouteListResponse),
        (status = 400, description = "Malformed query parameters, or an invalid cursor or page size", body = Problem, content_type = "application/problem+json"),
        (status = 401, body = Problem, content_type = "application/problem+json"),
        (status = 403, body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [])))]
pub(crate) async fn list_routes(
    State(state): State<ManagementState>,
    Query(query): Query<PageQuery>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<RouteListResponse>, Problem> {
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let routes =
        crate::routes::repository::list_routes(&state.request_boundary.pool, cursor, limit)
            .await
            .map_err(map_configuration)?;
    Ok(Json(RouteListResponse {
        items: routes.items.into_iter().map(Into::into).collect(),
        next_cursor: routes.next_cursor.map(|value| value.to_string()),
    }))
}

#[utoipa::path(
    get,
    path = "/api/v3/routes/{route_id}",
    tag = "routes",
    params(("route_id" = Uuid, Path)),
    responses((status = 200, body = RouteDetailResponse), (status = 404, body = Problem, content_type = "application/problem+json"))
, security(("sessionCookie" = [])))]
pub(crate) async fn get_route(
    State(state): State<ManagementState>,
    Path(route_id): Path<Uuid>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<RouteDetailResponse>, Problem> {
    require_permission(&principal, Permission::ReadConfiguration)?;
    let route = crate::routes::repository::get_route(&state.request_boundary.pool, route_id)
        .await
        .map_err(map_configuration)?;
    Ok(Json(route.into()))
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct RouteRevisionListResponse {
    pub items: Vec<RouteRevisionResponse>,
    pub next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v3/routes/{route_id}/revisions",
    tag = "routes",
    params(
        ("route_id" = Uuid, Path),
        PageQuery,
    ),
    responses(
        (status = 200, body = RouteRevisionListResponse),
        (status = 400, description = "Malformed query parameters, or an invalid cursor or page size", body = Problem, content_type = "application/problem+json"),
        (status = 404, body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [])))]
pub(crate) async fn list_route_revisions(
    State(state): State<ManagementState>,
    Path(route_id): Path<Uuid>,
    Query(query): Query<PageQuery>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<RouteRevisionListResponse>, Problem> {
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let page = crate::routes::revisions::list_route_revisions(
        &state.request_boundary.pool,
        route_id,
        cursor,
        limit,
    )
    .await
    .map_err(map_configuration)?;
    let items = page.items.into_iter().map(Into::into).collect();
    Ok(Json(RouteRevisionListResponse {
        items,
        next_cursor: page.next_cursor.map(|cursor| cursor.to_string()),
    }))
}

#[utoipa::path(
    get,
    path = "/api/v3/routes/{route_id}/revisions/{revision_id}",
    tag = "routes",
    params(("route_id" = Uuid, Path), ("revision_id" = Uuid, Path)),
    responses((status = 200, body = RouteRevisionResponse), (status = 404, body = Problem, content_type = "application/problem+json"))
, security(("sessionCookie" = [])))]
pub(crate) async fn get_route_revision(
    State(state): State<ManagementState>,
    Path((route_id, revision_id)): Path<(Uuid, Uuid)>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<RouteRevisionResponse>, Problem> {
    require_permission(&principal, Permission::ReadConfiguration)?;
    Ok(Json(
        crate::routes::revisions::get_route_revision(
            &state.request_boundary.pool,
            route_id,
            revision_id,
        )
        .await
        .map_err(map_configuration)?
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
    path = "/api/v3/routes/{route_id}/revisions/diff",
    tag = "routes",
    params(("route_id" = Uuid, Path), ("from" = Uuid, Query), ("to" = Uuid, Query)),
    responses((status = 200, body = RouteRevisionDiffResponse), (status = 404, body = Problem, content_type = "application/problem+json"))
, security(("sessionCookie" = [])))]
pub(crate) async fn diff_route_revisions(
    State(state): State<ManagementState>,
    Path(route_id): Path<Uuid>,
    Query(query): Query<DiffQuery>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<RouteRevisionDiffResponse>, Problem> {
    require_permission(&principal, Permission::ReadConfiguration)?;
    Ok(Json(
        crate::routes::revisions::diff_route_revisions(
            &state.request_boundary.pool,
            route_id,
            query.from,
            query.to,
        )
        .await
        .map_err(map_configuration)?
        .into(),
    ))
}

#[utoipa::path(
    post,
    path = "/api/v3/routes/{route_id}/revisions/{revision_id}/restore-as-draft",
    tag = "routes",
    params(("route_id" = Uuid, Path), ("revision_id" = Uuid, Path), ("Idempotency-Key" = String, Header)),
    responses(
        (status = 201, body = RouteDraftDetailResponse, headers(("Location" = String, description = "Path of the created resource"))),
        (status = 400, description = "Idempotency-Key is missing or invalid", body = Problem, content_type = "application/problem+json"),
        (status = 409, description = "Idempotency-Key was already used or is in progress", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [], "csrfToken" = [])))]
pub(crate) async fn restore_route_revision(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    Path((route_id, revision_id)): Path<(Uuid, Uuid)>,
    headers: HeaderMap,
    MutationPrincipal(principal): MutationPrincipal,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ManageRoutes)?;
    let state = &state;
    let provenance = &provenance;
    ReplayableMutation::new(
        state,
        principal.user_id,
        operations::ROUTE_RESTORE_AS_DRAFT,
        &headers,
        &RestoreRouteRevisionFingerprint {
            route_id,
            revision_id,
        },
    )?
    .run(|key| async move {
        let draft: RouteDraftDetailResponse =
            crate::routes::revisions::restore_route_revision_as_draft(
                &state.request_boundary.pool,
                provenance,
                route_id,
                revision_id,
                principal.user_id,
                &key,
            )
            .await
            .map_err(map_configuration)?
            .into();
        let etag = draft.etag;
        let location = format!("/api/v3/route-drafts/{}", draft.id);
        Ok(MutationReply {
            status: StatusCode::CREATED,
            body: draft,
            etag: Some(etag),
            location: Some(location),
        })
    })
    .await
}

#[derive(Serialize)]
struct RestoreRouteRevisionFingerprint {
    route_id: Uuid,
    revision_id: Uuid,
}
