use axum::{
    Json,
    extract::{Path, Query, State, rejection::JsonRejection},
    http::{HeaderMap, StatusCode},
    response::Response,
};
use chrono::{DateTime, Utc};
use olp_domain::ScopedRole;
use olp_storage::{
    idempotency::{IdempotencyResponse, ReplayableIdempotency, idempotency_fingerprint},
    identity::{
        NewProject, NewServiceAccount, NewTeam, ProjectRecord, RuntimeGenerationRecord,
        ScopedMembershipRecord, ServiceAccountRecord, TeamRecord,
    },
};
use serde::{Deserialize, Serialize};
use utoipa::{IntoParams, ToSchema};
use uuid::Uuid;

use crate::{ManagementState, Problem};

use super::super::{
    PageQuery, Permission, RuntimeGenerationResponse, idempotency_http_response, if_match,
    json_payload, map_persistence, optional_if_match, page, require_idempotency_key,
    require_mutation_session, require_permission, require_read_session, with_etag,
};
use crate::management::error_mapping::map_identity;

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct TeamResponse {
    pub id: Uuid,
    pub name: String,
    pub active: bool,
    pub etag: Uuid,
    pub created_by: Uuid,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

impl From<TeamRecord> for TeamResponse {
    fn from(value: TeamRecord) -> Self {
        Self {
            id: value.id,
            name: value.name,
            active: value.active,
            etag: value.etag,
            created_by: value.created_by,
            created_at: value.created_at,
            updated_at: value.updated_at,
        }
    }
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ProjectResponse {
    pub id: Uuid,
    pub team_id: Uuid,
    pub name: String,
    pub active: bool,
    pub etag: Uuid,
    pub created_by: Uuid,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

impl From<ProjectRecord> for ProjectResponse {
    fn from(value: ProjectRecord) -> Self {
        Self {
            id: value.id,
            team_id: value.team_id,
            name: value.name,
            active: value.active,
            etag: value.etag,
            created_by: value.created_by,
            created_at: value.created_at,
            updated_at: value.updated_at,
        }
    }
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ServiceAccountResponse {
    pub id: Uuid,
    pub team_id: Uuid,
    pub project_id: Uuid,
    pub name: String,
    pub active: bool,
    pub etag: Uuid,
    pub created_by: Uuid,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

impl From<ServiceAccountRecord> for ServiceAccountResponse {
    fn from(value: ServiceAccountRecord) -> Self {
        Self {
            id: value.id,
            team_id: value.team_id,
            project_id: value.project_id,
            name: value.name,
            active: value.active,
            etag: value.etag,
            created_by: value.created_by,
            created_at: value.created_at,
            updated_at: value.updated_at,
        }
    }
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct MembershipResponse {
    pub team_id: Uuid,
    pub project_id: Option<Uuid>,
    pub user_id: Uuid,
    pub role: ScopedRole,
    pub etag: Uuid,
    pub created_by: Uuid,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

impl From<ScopedMembershipRecord> for MembershipResponse {
    fn from(value: ScopedMembershipRecord) -> Self {
        Self {
            team_id: value.team_id,
            project_id: value.project_id,
            user_id: value.user_id,
            role: value.role,
            etag: value.etag,
            created_by: value.created_by,
            created_at: value.created_at,
            updated_at: value.updated_at,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct TeamListResponse {
    pub items: Vec<TeamResponse>,
    pub next_cursor: Option<String>,
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct ProjectListResponse {
    pub items: Vec<ProjectResponse>,
    pub next_cursor: Option<String>,
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct ServiceAccountListResponse {
    pub items: Vec<ServiceAccountResponse>,
    pub next_cursor: Option<String>,
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct MembershipListResponse {
    pub items: Vec<MembershipResponse>,
}

#[derive(Debug, Deserialize, Serialize, ToSchema)]
pub(crate) struct CreateTeamRequest {
    pub name: String,
}

#[derive(Debug, Deserialize, Serialize, ToSchema)]
pub(crate) struct CreateProjectRequest {
    pub team_id: Uuid,
    pub name: String,
}

#[derive(Debug, Deserialize, Serialize, ToSchema)]
pub(crate) struct CreateServiceAccountRequest {
    pub team_id: Uuid,
    pub project_id: Uuid,
    pub name: String,
}

#[derive(Debug, Deserialize, ToSchema)]
pub(crate) struct UpdateScopedResourceRequest {
    pub name: Option<String>,
    pub active: Option<bool>,
}

#[derive(Debug, Deserialize, ToSchema)]
pub(crate) struct PutMembershipRequest {
    pub role: ScopedRole,
}

#[derive(Debug, Deserialize, IntoParams)]
pub(crate) struct ProjectListQuery {
    pub team_id: Option<Uuid>,
    pub cursor: Option<String>,
    pub limit: Option<u16>,
}

#[derive(Debug, Deserialize, IntoParams)]
pub(crate) struct ServiceAccountListQuery {
    pub project_id: Option<Uuid>,
    pub cursor: Option<String>,
    pub limit: Option<u16>,
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct TeamMutationResponse {
    pub team: TeamResponse,
    pub runtime_generation: Option<RuntimeGenerationResponse>,
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct ProjectMutationResponse {
    pub project: ProjectResponse,
    pub runtime_generation: Option<RuntimeGenerationResponse>,
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct ServiceAccountMutationResponse {
    pub service_account: ServiceAccountResponse,
    pub runtime_generation: Option<RuntimeGenerationResponse>,
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct MembershipRemovalResponse {
    pub runtime_generation: Option<RuntimeGenerationResponse>,
}

fn runtime_generation(
    generation: Option<RuntimeGenerationRecord>,
) -> Option<RuntimeGenerationResponse> {
    generation.map(|generation| RuntimeGenerationResponse {
        id: generation.id,
        sequence: generation.sequence,
    })
}

fn creation_replay<'a>(
    state: &'a ManagementState,
    fingerprint: [u8; 32],
) -> Result<ReplayableIdempotency<'a>, Problem> {
    let master_key = state
        .master_key
        .as_deref()
        .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))?;
    Ok(ReplayableIdempotency::new(fingerprint, master_key))
}

#[utoipa::path(get, path = "/api/v1/teams", tag = "teams", params(PageQuery), responses((status = 200, body = TeamListResponse)))]
pub(crate) async fn list_teams(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<TeamListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let mut items = state
        .store()
        .list_teams(principal.user_id, cursor, limit + 1)
        .await
        .map_err(map_identity)?;
    let next_cursor =
        (items.len() > limit as usize).then(|| items[limit as usize - 1].id.to_string());
    items.truncate(limit as usize);
    Ok(Json(TeamListResponse {
        items: items.into_iter().map(Into::into).collect(),
        next_cursor,
    }))
}

#[utoipa::path(get, path = "/api/v1/teams/{team_id}", tag = "teams", params(("team_id" = Uuid, Path)), responses((status = 200, body = TeamResponse), (status = 404, body = Problem)))]
pub(crate) async fn get_team(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Path(team_id): Path<Uuid>,
) -> Result<Response, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let record = state
        .store()
        .get_team(principal.user_id, team_id)
        .await
        .map_err(map_identity)?;
    let etag = record.etag;
    with_etag(Json(TeamResponse::from(record)), etag)
}

#[utoipa::path(post, path = "/api/v1/teams", tag = "teams", request_body = CreateTeamRequest, responses((status = 201, body = TeamResponse), (status = 403, body = Problem), (status = 409, body = Problem)))]
pub(crate) async fn create_team(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    payload: Result<Json<CreateTeamRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageApiKeys)?;
    let idempotency_key = require_idempotency_key(&headers)?.to_owned();
    let request = json_payload(payload)?;
    let fingerprint = idempotency_fingerprint(&request).map_err(map_persistence)?;
    let outcome = state
        .store()
        .create_team(
            NewTeam {
                name: request.name,
                actor: principal.user_id,
                idempotency_key,
            },
            creation_replay(&state, fingerprint)?,
            |record| {
                IdempotencyResponse::json(
                    StatusCode::CREATED.as_u16(),
                    &TeamResponse::from(record.clone()),
                    Some(format!("\"{}\"", record.etag)),
                )
            },
        )
        .await
        .map_err(map_identity)?;
    idempotency_http_response(outcome)
}

#[utoipa::path(patch, path = "/api/v1/teams/{team_id}", tag = "teams", request_body = UpdateScopedResourceRequest, params(("team_id" = Uuid, Path)), responses((status = 200, body = TeamMutationResponse), (status = 404, body = Problem), (status = 412, body = Problem)))]
pub(crate) async fn update_team(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Path(team_id): Path<Uuid>,
    payload: Result<Json<UpdateScopedResourceRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageApiKeys)?;
    let expected = if_match(&headers)?;
    let idempotency_key = require_idempotency_key(&headers)?;
    let request = json_payload(payload)?;
    let updated = state
        .store()
        .update_team(
            team_id,
            request.name.as_deref(),
            request.active,
            expected,
            principal.user_id,
            idempotency_key,
        )
        .await
        .map_err(map_identity)?;
    let etag = updated.resource.etag;
    with_etag(
        Json(TeamMutationResponse {
            team: updated.resource.into(),
            runtime_generation: runtime_generation(updated.runtime_generation),
        }),
        etag,
    )
}

#[utoipa::path(get, path = "/api/v1/projects", tag = "projects", params(ProjectListQuery), responses((status = 200, body = ProjectListResponse)))]
pub(crate) async fn list_projects(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Query(query): Query<ProjectListQuery>,
) -> Result<Json<ProjectListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(PageQuery {
        cursor: query.cursor,
        limit: query.limit,
    })?;
    let mut items = state
        .store()
        .list_projects(principal.user_id, query.team_id, cursor, limit + 1)
        .await
        .map_err(map_identity)?;
    let next_cursor =
        (items.len() > limit as usize).then(|| items[limit as usize - 1].id.to_string());
    items.truncate(limit as usize);
    Ok(Json(ProjectListResponse {
        items: items.into_iter().map(Into::into).collect(),
        next_cursor,
    }))
}

#[utoipa::path(get, path = "/api/v1/projects/{project_id}", tag = "projects", params(("project_id" = Uuid, Path)), responses((status = 200, body = ProjectResponse), (status = 404, body = Problem)))]
pub(crate) async fn get_project(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Path(project_id): Path<Uuid>,
) -> Result<Response, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let record = state
        .store()
        .get_project(principal.user_id, project_id)
        .await
        .map_err(map_identity)?;
    let etag = record.etag;
    with_etag(Json(ProjectResponse::from(record)), etag)
}

#[utoipa::path(post, path = "/api/v1/projects", tag = "projects", request_body = CreateProjectRequest, responses((status = 201, body = ProjectResponse), (status = 403, body = Problem), (status = 409, body = Problem)))]
pub(crate) async fn create_project(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    payload: Result<Json<CreateProjectRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageApiKeys)?;
    let idempotency_key = require_idempotency_key(&headers)?.to_owned();
    let request = json_payload(payload)?;
    let fingerprint = idempotency_fingerprint(&request).map_err(map_persistence)?;
    let outcome = state
        .store()
        .create_project(
            NewProject {
                team_id: request.team_id,
                name: request.name,
                actor: principal.user_id,
                idempotency_key,
            },
            creation_replay(&state, fingerprint)?,
            |record| {
                IdempotencyResponse::json(
                    StatusCode::CREATED.as_u16(),
                    &ProjectResponse::from(record.clone()),
                    Some(format!("\"{}\"", record.etag)),
                )
            },
        )
        .await
        .map_err(map_identity)?;
    idempotency_http_response(outcome)
}

#[utoipa::path(patch, path = "/api/v1/projects/{project_id}", tag = "projects", request_body = UpdateScopedResourceRequest, params(("project_id" = Uuid, Path)), responses((status = 200, body = ProjectMutationResponse), (status = 404, body = Problem), (status = 412, body = Problem)))]
pub(crate) async fn update_project(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Path(project_id): Path<Uuid>,
    payload: Result<Json<UpdateScopedResourceRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageApiKeys)?;
    let expected = if_match(&headers)?;
    let idempotency_key = require_idempotency_key(&headers)?;
    let request = json_payload(payload)?;
    let updated = state
        .store()
        .update_project(
            project_id,
            request.name.as_deref(),
            request.active,
            expected,
            principal.user_id,
            idempotency_key,
        )
        .await
        .map_err(map_identity)?;
    let etag = updated.resource.etag;
    with_etag(
        Json(ProjectMutationResponse {
            project: updated.resource.into(),
            runtime_generation: runtime_generation(updated.runtime_generation),
        }),
        etag,
    )
}

#[utoipa::path(get, path = "/api/v1/service-accounts", tag = "service-accounts", params(ServiceAccountListQuery), responses((status = 200, body = ServiceAccountListResponse)))]
pub(crate) async fn list_service_accounts(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Query(query): Query<ServiceAccountListQuery>,
) -> Result<Json<ServiceAccountListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(PageQuery {
        cursor: query.cursor,
        limit: query.limit,
    })?;
    let mut items = state
        .store()
        .list_service_accounts(principal.user_id, query.project_id, cursor, limit + 1)
        .await
        .map_err(map_identity)?;
    let next_cursor =
        (items.len() > limit as usize).then(|| items[limit as usize - 1].id.to_string());
    items.truncate(limit as usize);
    Ok(Json(ServiceAccountListResponse {
        items: items.into_iter().map(Into::into).collect(),
        next_cursor,
    }))
}

#[utoipa::path(get, path = "/api/v1/service-accounts/{service_account_id}", tag = "service-accounts", params(("service_account_id" = Uuid, Path)), responses((status = 200, body = ServiceAccountResponse), (status = 404, body = Problem)))]
pub(crate) async fn get_service_account(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Path(service_account_id): Path<Uuid>,
) -> Result<Response, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let record = state
        .store()
        .get_service_account(principal.user_id, service_account_id)
        .await
        .map_err(map_identity)?;
    let etag = record.etag;
    with_etag(Json(ServiceAccountResponse::from(record)), etag)
}

#[utoipa::path(post, path = "/api/v1/service-accounts", tag = "service-accounts", request_body = CreateServiceAccountRequest, responses((status = 201, body = ServiceAccountResponse), (status = 403, body = Problem), (status = 409, body = Problem)))]
pub(crate) async fn create_service_account(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    payload: Result<Json<CreateServiceAccountRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageApiKeys)?;
    let idempotency_key = require_idempotency_key(&headers)?.to_owned();
    let request = json_payload(payload)?;
    let fingerprint = idempotency_fingerprint(&request).map_err(map_persistence)?;
    let outcome = state
        .store()
        .create_service_account(
            NewServiceAccount {
                team_id: request.team_id,
                project_id: request.project_id,
                name: request.name,
                actor: principal.user_id,
                idempotency_key,
            },
            creation_replay(&state, fingerprint)?,
            |record| {
                IdempotencyResponse::json(
                    StatusCode::CREATED.as_u16(),
                    &ServiceAccountResponse::from(record.clone()),
                    Some(format!("\"{}\"", record.etag)),
                )
            },
        )
        .await
        .map_err(map_identity)?;
    idempotency_http_response(outcome)
}

#[utoipa::path(patch, path = "/api/v1/service-accounts/{service_account_id}", tag = "service-accounts", request_body = UpdateScopedResourceRequest, params(("service_account_id" = Uuid, Path)), responses((status = 200, body = ServiceAccountMutationResponse), (status = 404, body = Problem), (status = 412, body = Problem)))]
pub(crate) async fn update_service_account(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Path(service_account_id): Path<Uuid>,
    payload: Result<Json<UpdateScopedResourceRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageApiKeys)?;
    let expected = if_match(&headers)?;
    let idempotency_key = require_idempotency_key(&headers)?;
    let request = json_payload(payload)?;
    let updated = state
        .store()
        .update_service_account(
            service_account_id,
            request.name.as_deref(),
            request.active,
            expected,
            principal.user_id,
            idempotency_key,
        )
        .await
        .map_err(map_identity)?;
    let etag = updated.resource.etag;
    with_etag(
        Json(ServiceAccountMutationResponse {
            service_account: updated.resource.into(),
            runtime_generation: runtime_generation(updated.runtime_generation),
        }),
        etag,
    )
}

#[utoipa::path(get, path = "/api/v1/teams/{team_id}/members", tag = "memberships", params(("team_id" = Uuid, Path)), responses((status = 200, body = MembershipListResponse), (status = 404, body = Problem)))]
pub(crate) async fn list_team_memberships(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Path(team_id): Path<Uuid>,
) -> Result<Json<MembershipListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let items = state
        .store()
        .list_team_memberships(principal.user_id, team_id)
        .await
        .map_err(map_identity)?;
    Ok(Json(MembershipListResponse {
        items: items.into_iter().map(Into::into).collect(),
    }))
}

#[utoipa::path(put, path = "/api/v1/teams/{team_id}/members/{user_id}", tag = "memberships", request_body = PutMembershipRequest, params(("team_id" = Uuid, Path), ("user_id" = Uuid, Path)), responses((status = 200, body = MembershipResponse), (status = 404, body = Problem), (status = 412, body = Problem)))]
pub(crate) async fn put_team_membership(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Path((team_id, user_id)): Path<(Uuid, Uuid)>,
    payload: Result<Json<PutMembershipRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageApiKeys)?;
    let expected = optional_if_match(&headers)?;
    let idempotency_key = require_idempotency_key(&headers)?;
    let request = json_payload(payload)?;
    let record = state
        .store()
        .put_team_membership(
            team_id,
            user_id,
            request.role,
            expected,
            principal.user_id,
            idempotency_key,
        )
        .await
        .map_err(map_identity)?;
    let etag = record.etag;
    with_etag(Json(MembershipResponse::from(record)), etag)
}

#[utoipa::path(delete, path = "/api/v1/teams/{team_id}/members/{user_id}", tag = "memberships", params(("team_id" = Uuid, Path), ("user_id" = Uuid, Path)), responses((status = 200, body = MembershipRemovalResponse), (status = 404, body = Problem), (status = 412, body = Problem)))]
pub(crate) async fn remove_team_membership(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Path((team_id, user_id)): Path<(Uuid, Uuid)>,
) -> Result<Json<MembershipRemovalResponse>, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageApiKeys)?;
    let expected = if_match(&headers)?;
    let idempotency_key = require_idempotency_key(&headers)?;
    let generation = state
        .store()
        .remove_team_membership(
            team_id,
            user_id,
            expected,
            principal.user_id,
            idempotency_key,
        )
        .await
        .map_err(map_identity)?;
    Ok(Json(MembershipRemovalResponse {
        runtime_generation: runtime_generation(generation),
    }))
}

#[utoipa::path(get, path = "/api/v1/projects/{project_id}/members", tag = "memberships", params(("project_id" = Uuid, Path)), responses((status = 200, body = MembershipListResponse), (status = 404, body = Problem)))]
pub(crate) async fn list_project_memberships(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Path(project_id): Path<Uuid>,
) -> Result<Json<MembershipListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let items = state
        .store()
        .list_project_memberships(principal.user_id, project_id)
        .await
        .map_err(map_identity)?;
    Ok(Json(MembershipListResponse {
        items: items.into_iter().map(Into::into).collect(),
    }))
}

#[utoipa::path(put, path = "/api/v1/projects/{project_id}/members/{user_id}", tag = "memberships", request_body = PutMembershipRequest, params(("project_id" = Uuid, Path), ("user_id" = Uuid, Path)), responses((status = 200, body = MembershipResponse), (status = 404, body = Problem), (status = 412, body = Problem)))]
pub(crate) async fn put_project_membership(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Path((project_id, user_id)): Path<(Uuid, Uuid)>,
    payload: Result<Json<PutMembershipRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageApiKeys)?;
    let expected = optional_if_match(&headers)?;
    let idempotency_key = require_idempotency_key(&headers)?;
    let request = json_payload(payload)?;
    let record = state
        .store()
        .put_project_membership(
            project_id,
            user_id,
            request.role,
            expected,
            principal.user_id,
            idempotency_key,
        )
        .await
        .map_err(map_identity)?;
    let etag = record.etag;
    with_etag(Json(MembershipResponse::from(record)), etag)
}

#[utoipa::path(delete, path = "/api/v1/projects/{project_id}/members/{user_id}", tag = "memberships", params(("project_id" = Uuid, Path), ("user_id" = Uuid, Path)), responses((status = 200, body = MembershipRemovalResponse), (status = 404, body = Problem), (status = 412, body = Problem)))]
pub(crate) async fn remove_project_membership(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Path((project_id, user_id)): Path<(Uuid, Uuid)>,
) -> Result<Json<MembershipRemovalResponse>, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageApiKeys)?;
    let expected = if_match(&headers)?;
    let idempotency_key = require_idempotency_key(&headers)?;
    let generation = state
        .store()
        .remove_project_membership(
            project_id,
            user_id,
            expected,
            principal.user_id,
            idempotency_key,
        )
        .await
        .map_err(map_identity)?;
    Ok(Json(MembershipRemovalResponse {
        runtime_generation: runtime_generation(generation),
    }))
}
