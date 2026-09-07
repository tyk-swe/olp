use crate::access::policy::Permission;
use crate::usage::ingestion::delivery_health::ConsumerStatus;
use crate::usage::ingestion::reconciliation::EpochAcknowledgement;
use crate::usage::ingestion::reconciliation::GatewayEpochRecord;
use crate::usage::ingestion::reconciliation::GatewayEpochState;
use axum::Json;
use axum::extract::Path;
use axum::extract::Query;
use axum::extract::State;
use chrono::DateTime;
use chrono::Utc;
use serde::Deserialize;
use serde::Serialize;
use utoipa::IntoParams;
use utoipa::ToSchema;
use uuid::Uuid;

use crate::access::permissions::require_permission;
use crate::access::principal::MutationPrincipal;
use crate::access::principal::ReadPrincipal;
use crate::http::control::error_mapping::map_persistence;
use crate::http::control::operations::helpers::map_operations;
use crate::http::control::operations::helpers::not_found;
use crate::http::control::operations::helpers::timestamp_cursor;
use crate::http::control::pagination::page_limit;
use crate::http::control::provenance::Provenance;
use crate::http::control::state::ManagementState;
use crate::http::problem::Problem;

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct RequestMetadataConsumerStatusResponse {
    state: String,
    pending_events: u64,
    lag_events: u64,
    oldest_pending_at: Option<DateTime<Utc>>,
    checked_at: Option<DateTime<Utc>>,
    heartbeat_age_seconds: Option<u64>,
}

impl From<ConsumerStatus> for RequestMetadataConsumerStatusResponse {
    fn from(consumer: ConsumerStatus) -> Self {
        Self {
            state: consumer.state.as_str().to_owned(),
            pending_events: consumer.pending_events,
            lag_events: consumer.lag_events,
            oldest_pending_at: consumer.oldest_pending_at,
            checked_at: consumer.checked_at,
            heartbeat_age_seconds: consumer.heartbeat_age_seconds,
        }
    }
}

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub(crate) struct RequestMetadataGatewayEpochQuery {
    cursor: Option<String>,
    #[param(minimum = 1, maximum = 200)]
    limit: Option<u16>,
    /// Filter by open, gracefully_closed, unresolved, or acknowledged.
    state: Option<String>,
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct RequestMetadataGatewayEpochResponse {
    gateway_instance: String,
    #[schema(value_type = String, format = Uuid)]
    process_epoch: Uuid,
    state: String,
    started_at: DateTime<Utc>,
    updated_at: DateTime<Utc>,
    accepted: u64,
    persisted: u64,
    dropped: u64,
    abandoned: u64,
    uncertain_event_lower_bound: u64,
    retrying: bool,
    writer_closed: bool,
    gracefully_closed_at: Option<DateTime<Utc>>,
    stale_detected_at: Option<DateTime<Utc>>,
    acknowledged_at: Option<DateTime<Utc>>,
    #[schema(value_type = Option<String>, format = Uuid)]
    acknowledged_by: Option<Uuid>,
    /// Ingestion gap opened for the events this epoch could not account for.
    #[schema(value_type = Option<String>, format = Uuid)]
    uncertainty_gap_id: Option<Uuid>,
}

impl From<GatewayEpochRecord> for RequestMetadataGatewayEpochResponse {
    fn from(value: GatewayEpochRecord) -> Self {
        Self {
            gateway_instance: value.gateway_instance,
            process_epoch: value.process_epoch,
            state: value.state.as_str().to_owned(),
            started_at: value.started_at,
            updated_at: value.updated_at,
            accepted: value.accepted,
            persisted: value.persisted,
            dropped: value.dropped,
            abandoned: value.abandoned,
            uncertain_event_lower_bound: value.uncertain_event_lower_bound,
            retrying: value.retrying,
            writer_closed: value.writer_closed,
            gracefully_closed_at: value.gracefully_closed_at,
            stale_detected_at: value.stale_detected_at,
            acknowledged_at: value.acknowledged_at,
            acknowledged_by: value.acknowledged_by,
            uncertainty_gap_id: value.uncertainty_gap_id,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct RequestMetadataGatewayEpochListResponse {
    data: Vec<RequestMetadataGatewayEpochResponse>,
    items: Vec<RequestMetadataGatewayEpochResponse>,
    next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v3/request-metadata/gateway-epochs",
    tag = "request-metadata",
    params(RequestMetadataGatewayEpochQuery),
    responses(
        (status = 200, description = "Request metadata gateway process epoch page", body = RequestMetadataGatewayEpochListResponse),
        (status = 400, description = "Invalid cursor or state filter", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [])))]
pub(crate) async fn list_request_metadata_gateway_epochs(
    State(state): State<ManagementState>,
    Query(query): Query<RequestMetadataGatewayEpochQuery>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<RequestMetadataGatewayEpochListResponse>, Problem> {
    require_permission(&principal, Permission::ReadOperations)?;
    let cursor = timestamp_cursor(query.cursor.as_deref())?;
    let state_filter = query
        .state
        .as_deref()
        .map(parse_request_metadata_gateway_epoch_state)
        .transpose()?;
    let page = crate::usage::ingestion::delivery_health::request_metadata_gateway_epochs(
        &state.request_boundary.pool,
        state_filter,
        cursor.as_ref(),
        page_limit(query.limit)?,
    )
    .await
    .map_err(map_operations)?;
    let items = page.items.into_iter().map(Into::into).collect::<Vec<_>>();
    Ok(Json(RequestMetadataGatewayEpochListResponse {
        data: items.clone(),
        items,
        next_cursor: page.next_cursor,
    }))
}

fn parse_request_metadata_gateway_epoch_state(value: &str) -> Result<GatewayEpochState, Problem> {
    match value {
        "open" => Ok(GatewayEpochState::Open),
        "gracefully_closed" => Ok(GatewayEpochState::GracefullyClosed),
        "unresolved" => Ok(GatewayEpochState::Unresolved),
        "acknowledged" => Ok(GatewayEpochState::Acknowledged),
        _ => Err(Problem::field_validation(
            "state",
            "Use open, gracefully_closed, unresolved, or acknowledged.",
        )),
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct RequestMetadataEpochAcknowledgementResponse {
    #[schema(value_type = String, format = Uuid)]
    process_epoch: Uuid,
    gateway_instance: String,
    acknowledged_at: DateTime<Utc>,
    #[schema(value_type = Option<String>, format = Uuid)]
    acknowledged_by: Option<Uuid>,
}

impl From<EpochAcknowledgement> for RequestMetadataEpochAcknowledgementResponse {
    fn from(value: EpochAcknowledgement) -> Self {
        Self {
            process_epoch: value.process_epoch,
            gateway_instance: value.gateway_instance,
            acknowledged_at: value.acknowledged_at,
            acknowledged_by: value.acknowledged_by,
        }
    }
}

#[utoipa::path(
    post,
    path = "/api/v3/request-metadata/gateway-epochs/{process_epoch}/acknowledge",
    tag = "request-metadata",
    params(("process_epoch" = Uuid, Path)),
    responses(
        (status = 200, description = "Unclean gateway epoch acknowledged; retained completeness evidence is unchanged", body = RequestMetadataEpochAcknowledgementResponse),
        (status = 404, description = "Unclean gateway epoch not found", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [], "csrfToken" = [])))]
pub(crate) async fn acknowledge_request_metadata_gateway_epoch(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    Path(process_epoch): Path<Uuid>,
    MutationPrincipal(principal): MutationPrincipal,
) -> Result<Json<RequestMetadataEpochAcknowledgementResponse>, Problem> {
    require_permission(&principal, Permission::ManageSettings)?;
    let acknowledgement =
        crate::usage::ingestion::reconciliation::acknowledge_request_metadata_gateway_epoch(
            &state.request_boundary.pool,
            &provenance,
            process_epoch,
            principal.user_id,
        )
        .await
        .map_err(map_persistence)?
        .ok_or_else(not_found)?;
    Ok(Json(acknowledgement.into()))
}

pub(crate) fn router()
-> utoipa_axum::router::OpenApiRouter<crate::http::control::state::ManagementState> {
    utoipa_axum::router::OpenApiRouter::new()
        .routes(utoipa_axum::routes!(
            crate::usage::delivery_http::acknowledge_request_metadata_gateway_epoch
        ))
        .routes(utoipa_axum::routes!(
            crate::usage::delivery_http::list_request_metadata_gateway_epochs
        ))
}
