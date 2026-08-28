use axum::{
    Json,
    extract::{Path, State, rejection::JsonRejection},
    http::{HeaderMap, StatusCode},
    response::Response,
};
use olp_db::{
    configuration::NewRouteDraft, configuration::NewRouteTarget, idempotency::Replayable,
    idempotency::Response as IdempotencyResponse, idempotency::fingerprint,
    idempotency::operations,
};
use olp_engine::domain::canonical::identity::OperationKind;
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;
use uuid::Uuid;

use crate::management::{
    error_mapping::{map_configuration, map_persistence},
    idempotency::{
        MutationReply, ReplayableMutation, idempotency_http_response, require_idempotency_key,
    },
    json_payload::json_payload,
    permissions::require_route_manager,
    preconditions::{if_match, with_etag},
    provenance::Provenance,
    response_policy::RuntimeGenerationResponse,
    sessions::require_mutation_session,
};
use crate::{bootstrap::mode_dependencies::ManagementState, public_http::problem::Problem};

#[derive(Debug, Deserialize, Serialize, ToSchema)]
pub(crate) struct CreateRouteDraftRequest {
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
pub(crate) struct RouteTargetRequest {
    #[schema(value_type = String, format = Uuid)]
    pub provider_id: Uuid,
    pub provider_model: String,
    pub priority: u16,
    pub weight: u32,
    pub timeout_ms: u64,
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct RouteDraftResponse {
    #[schema(value_type = String, format = Uuid)]
    pub id: Uuid,
    pub slug: String,
    pub state: String,
    #[schema(value_type = String, format = Uuid)]
    pub etag: Uuid,
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct RouteActivationResponse {
    #[schema(value_type = String, format = Uuid)]
    pub route_id: Uuid,
    #[schema(value_type = String, format = Uuid)]
    pub revision_id: Uuid,
    pub revision: i32,
    /// The activated draft returns to `draft` under this ETag; revalidate
    /// before activating it again.
    #[schema(value_type = String, format = Uuid)]
    pub draft_etag: Uuid,
    pub runtime_generation: RuntimeGenerationResponse,
}

#[utoipa::path(
    post,
    path = "/api/v1/route-drafts",
    tag = "routes",
    request_body = CreateRouteDraftRequest,
    params(("Idempotency-Key" = String, Header, description = "Unique route-draft creation key")),
    responses(
        (status = 201, description = "Route draft created", body = RouteDraftResponse, headers(("Location" = String, description = "Path of the created resource"))),
        (status = 400, description = "Idempotency-Key is missing or invalid", body = Problem),
        (status = 409, description = "Idempotency-Key was already used or is in progress", body = Problem),
        (status = 422, description = "Route draft is invalid", body = Problem),
        (status = 503, description = "Master key or database unavailable", body = Problem)
    )
)]
pub(crate) async fn create_route_draft(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    headers: HeaderMap,
    payload: Result<Json<CreateRouteDraftRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_route_manager(&principal)?;
    let idempotency_key = require_idempotency_key(&headers)?.to_owned();
    let request = json_payload(payload)?;
    let request_fingerprint = fingerprint(&request).map_err(map_persistence)?;
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
            Problem::field_validation(
                "operations",
                format!("Operation {operation} is not supported by the operation model."),
            )
        })?;
    let targets = request
        .targets
        .into_iter()
        .map(|target| NewRouteTarget {
            provider_id: target.provider_id,
            upstream_model: target.provider_model,
            priority: target.priority,
            weight: target.weight,
            timeout_ms: target.timeout_ms,
        })
        .collect();
    let created = state
        .store()
        .with_provenance(&provenance)
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
            Replayable::new(request_fingerprint, master_key),
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
                .and_then(|response| {
                    response.with_location(format!("/api/v1/route-drafts/{}", created.id))
                })
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
pub(crate) async fn validate_route_draft(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    Path(draft_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_route_manager(&principal)?;
    let (etag, slug) = state
        .store()
        .with_provenance(&provenance)
        .validate_route_draft(draft_id, if_match(&headers)?, principal.user_id)
        .await
        .map_err(map_configuration)?;
    with_etag(
        (
            StatusCode::OK,
            Json(RouteDraftResponse {
                id: draft_id,
                slug: slug.to_string(),
                state: "validated".to_owned(),
                etag,
            }),
        ),
        etag,
    )
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
        (status = 200, description = "Route activated, runtime published, and the draft returned to `draft` under a new ETag", body = RouteActivationResponse),
        (status = 400, description = "Idempotency-Key is missing or invalid", body = Problem),
        (status = 409, description = "Draft has not been validated, or the Idempotency-Key was already used for a different request", body = Problem),
        (status = 412, description = "ETag mismatch", body = Problem)
    )
)]
pub(crate) async fn activate_route_draft(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    Path(draft_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_route_manager(&principal)?;
    let expected_etag = if_match(&headers)?;
    let state = &state;
    let provenance = &provenance;
    ReplayableMutation::new(
        state,
        principal.user_id,
        operations::ROUTE_ACTIVATE,
        &headers,
        &ActivateRouteDraftFingerprint {
            draft_id,
            expected_etag,
        },
    )?
    .run(|key| async move {
        let activated = state
            .store()
            .with_provenance(provenance)
            .activate_route_draft(draft_id, expected_etag, principal.user_id, &key)
            .await
            .map_err(map_configuration)?;
        Ok(MutationReply {
            status: StatusCode::OK,
            body: RouteActivationResponse {
                route_id: activated.route_id,
                revision_id: activated.revision_id,
                revision: activated.revision,
                draft_etag: activated.draft_etag,
                runtime_generation: (&activated.release).into(),
            },
            etag: Some(activated.draft_etag),
            location: None,
        })
    })
    .await
}

#[derive(Serialize)]
struct ActivateRouteDraftFingerprint {
    draft_id: Uuid,
    expected_etag: Uuid,
}
