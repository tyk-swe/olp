use axum::{
    Json,
    extract::{Path, State, rejection::JsonRejection},
    http::{HeaderMap, HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
};
use olp_domain::OperationKind;
use olp_storage::{
    IdempotencyResponse, NewRouteDraft, NewRouteTarget, ReplayableIdempotency,
    idempotency_fingerprint,
};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;
use uuid::Uuid;

use super::common::*;
use crate::{ApiState, FieldErrors, Problem};

#[derive(Debug, Deserialize, Serialize, ToSchema)]
pub(super) struct CreateRouteDraftRequest {
    pub slug: String,
    #[serde(default = "default_route_operations")]
    pub operations: Vec<String>,
    pub overall_timeout_ms: u64,
    pub max_attempts: u16,
    pub targets: Vec<RouteTargetRequest>,
}

fn default_route_operations() -> Vec<String> {
    vec!["generation".to_owned()]
}

#[derive(Debug, Deserialize, Serialize, ToSchema)]
pub(super) struct RouteTargetRequest {
    #[schema(value_type = String, format = Uuid)]
    pub provider_id: Uuid,
    pub provider_model: String,
    pub priority: u16,
    pub weight: u32,
    pub timeout_ms: u64,
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct RouteDraftResponse {
    #[schema(value_type = String, format = Uuid)]
    pub id: Uuid,
    pub slug: String,
    pub state: String,
    #[schema(value_type = String, format = Uuid)]
    pub etag: Uuid,
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct RouteActivationResponse {
    #[schema(value_type = String, format = Uuid)]
    pub route_id: Uuid,
    #[schema(value_type = String, format = Uuid)]
    pub revision_id: Uuid,
    pub revision: i32,
    pub runtime_generation: RuntimeGenerationResponse,
}

#[utoipa::path(
    post,
    path = "/api/v1/route-drafts",
    tag = "routes",
    request_body = CreateRouteDraftRequest,
    params(("Idempotency-Key" = String, Header, description = "Unique route-draft creation key")),
    responses(
        (status = 201, description = "Route draft created", body = RouteDraftResponse),
        (status = 400, description = "Idempotency-Key is missing or invalid", body = Problem),
        (status = 409, description = "Idempotency-Key was already used or is in progress", body = Problem),
        (status = 422, description = "Route draft is invalid", body = Problem),
        (status = 503, description = "Master key or database unavailable", body = Problem)
    )
)]
pub(super) async fn create_route_draft(
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<CreateRouteDraftRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_route_manager(&principal)?;
    let idempotency_key = require_idempotency_key(&headers)?.to_owned();
    let request = json_payload(payload)?;
    let request_fingerprint = idempotency_fingerprint(&request).map_err(map_persistence)?;
    let master_key = state
        .master_key
        .as_deref()
        .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))?;
    let operations = request
        .operations
        .iter()
        .map(|operation| {
            operation
                .parse::<OperationKind>()
                .map_err(|_| operation.clone())
        })
        .collect::<Result<Vec<_>, _>>()
        .map_err(|operation| {
            let mut errors = FieldErrors::new();
            errors.insert(
                "operations".to_owned(),
                vec![format!(
                    "Operation {operation} is not supported by the operation model."
                )],
            );
            Problem::validation(errors)
        })?;
    let targets = request
        .targets
        .into_iter()
        .map(|target| NewRouteTarget {
            provider_id: target.provider_id,
            provider_model: target.provider_model,
            priority: target.priority,
            weight: target.weight,
            timeout_ms: target.timeout_ms,
        })
        .collect();
    let created = require_store(&state)?
        .create_route_draft(
            NewRouteDraft {
                slug: request.slug,
                operations,
                overall_timeout_ms: request.overall_timeout_ms,
                max_attempts: request.max_attempts,
                targets,
                actor: principal.user_id,
                idempotency_key,
            },
            ReplayableIdempotency::new(request_fingerprint, master_key),
            |created| {
                IdempotencyResponse::json(
                    StatusCode::CREATED.as_u16(),
                    &RouteDraftResponse {
                        id: created.id,
                        slug: created.slug.to_string(),
                        state: "draft".to_owned(),
                        etag: created.etag,
                    },
                    Some(format!("\"{}\"", created.etag)),
                )
            },
        )
        .await
        .map_err(map_configuration)?;
    idempotency_http_response(created)
}

#[utoipa::path(
    post,
    path = "/api/v1/route-drafts/{draft_id}/validate",
    tag = "routes",
    params(
        ("draft_id" = Uuid, Path, description = "Route draft ID"),
        ("If-Match" = String, Header, description = "Current route-draft ETag")
    ),
    responses(
        (status = 200, description = "Route draft validated", body = RouteDraftResponse),
        (status = 412, description = "ETag mismatch", body = Problem),
        (status = 422, description = "Eligibility validation failed", body = Problem)
    )
)]
pub(super) async fn validate_route_draft(
    State(state): State<ApiState>,
    Path(draft_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_route_manager(&principal)?;
    let (etag, slug) = require_store(&state)?
        .validate_route_draft(draft_id, if_match(&headers)?, principal.user_id)
        .await
        .map_err(map_configuration)?;
    let mut response = (
        StatusCode::OK,
        Json(RouteDraftResponse {
            id: draft_id,
            slug: slug.to_string(),
            state: "validated".to_owned(),
            etag,
        }),
    )
        .into_response();
    response.headers_mut().insert(
        header::ETAG,
        HeaderValue::from_str(&format!("\"{etag}\"")).map_err(|_| Problem::internal())?,
    );
    Ok(response)
}

#[utoipa::path(
    post,
    path = "/api/v1/route-drafts/{draft_id}/activate",
    tag = "routes",
    params(
        ("draft_id" = Uuid, Path, description = "Route draft ID"),
        ("If-Match" = String, Header, description = "Validated route-draft ETag"),
        ("Idempotency-Key" = String, Header, description = "Unique activation key")
    ),
    responses(
        (status = 200, description = "Route activated and runtime published", body = RouteActivationResponse),
        (status = 409, description = "Draft has not been validated", body = Problem),
        (status = 412, description = "ETag mismatch", body = Problem)
    )
)]
pub(super) async fn activate_route_draft(
    State(state): State<ApiState>,
    Path(draft_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_route_manager(&principal)?;
    let idempotency_key = require_idempotency_key(&headers)?;
    let expected_etag = if_match(&headers)?;
    let activated = require_store(&state)?
        .activate_route_draft(draft_id, expected_etag, principal.user_id, idempotency_key)
        .await
        .map_err(map_configuration)?;
    let mut response = Json(RouteActivationResponse {
        route_id: activated.route_id,
        revision_id: activated.revision_id,
        revision: activated.revision,
        runtime_generation: RuntimeGenerationResponse {
            id: activated.release.generation_id,
            sequence: activated.release.sequence,
        },
    })
    .into_response();
    response.headers_mut().insert(
        header::ETAG,
        HeaderValue::from_str(&format!("\"{expected_etag}\"")).map_err(|_| Problem::internal())?,
    );
    Ok(response)
}
