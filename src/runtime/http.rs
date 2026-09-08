use crate::access::policy::Permission;
use crate::runtime::history::GenerationRecord;
use axum::Json;
use axum::extract::Query;
use axum::extract::State;
use chrono::DateTime;
use chrono::Utc;
use serde::Serialize;
use utoipa::ToSchema;
use uuid::Uuid;

use crate::access::permissions::require_permission;
use crate::access::principal::ReadPrincipal;
use crate::http::control::operations::helpers::map_operations;
use crate::http::control::pagination::PageQuery;
use crate::http::control::pagination::page_limit;
use crate::http::control::state::ManagementState;
use crate::http::problem::Problem;

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct RuntimeGenerationItem {
    #[schema(value_type = String, format = Uuid)]
    id: Uuid,
    sequence: u64,
    sha256: String,
    #[schema(value_type = String, format = Uuid)]
    created_by: Uuid,
    created_by_email: String,
    created_at: DateTime<Utc>,
}

impl From<GenerationRecord> for RuntimeGenerationItem {
    fn from(record: GenerationRecord) -> Self {
        Self {
            id: record.id,
            sequence: record.sequence,
            sha256: record.sha256_hex,
            created_by: record.created_by,
            created_by_email: record.created_by_email,
            created_at: record.created_at,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct RuntimeGenerationListResponse {
    items: Vec<RuntimeGenerationItem>,
    next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v3/runtime-generations",
    tag = "runtime",
    params(PageQuery),
    responses(
        (status = 200, description = "Runtime generations", body = RuntimeGenerationListResponse),
        (status = 400, description = "Malformed query parameters, or an invalid cursor or page size", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [])))]
pub(crate) async fn list_runtime_generations(
    State(state): State<ManagementState>,
    Query(query): Query<PageQuery>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<RuntimeGenerationListResponse>, Problem> {
    require_permission(&principal, Permission::ReadOperations)?;
    let before = query
        .cursor
        .as_deref()
        .map(str::parse::<u64>)
        .transpose()
        .map_err(|_| Problem::bad_request("invalid_cursor", "The cursor is invalid."))?;
    let limit = page_limit(query.limit)?;
    let page =
        crate::runtime::history::runtime_generations(&state.request_boundary.pool, before, limit)
            .await
            .map_err(map_operations)?;
    let items = page.items.into_iter().map(Into::into).collect::<Vec<_>>();
    Ok(Json(RuntimeGenerationListResponse {
        items,
        next_cursor: page.next_cursor,
    }))
}

pub(crate) fn router()
-> utoipa_axum::router::OpenApiRouter<crate::http::control::state::ManagementState> {
    utoipa_axum::router::OpenApiRouter::new().routes(utoipa_axum::routes!(
        crate::runtime::http::list_runtime_generations
    ))
}
