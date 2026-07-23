use axum::{
    Json,
    extract::{Path, Query, State},
    http::HeaderMap,
};
use chrono::{DateTime, Utc};
use olp_storage::{
    RequestMetadataConsumerStatus, RequestMetadataEpochAcknowledgement,
    RequestMetadataGatewayEpochRecord, RequestMetadataGatewayEpochState, TimestampCursor,
};
use serde::{Deserialize, Serialize};
use utoipa::{IntoParams, ToSchema};
use uuid::Uuid;

use crate::{
    FieldErrors, ManagementState, Problem,
    management_api::{
        Permission, map_persistence, require_mutation_session, require_permission,
        require_read_session,
    },
    operations::helpers::{map_operations, not_found, page_limit},
};

#[derive(Debug, Serialize, ToSchema)]
pub(in crate::operations) struct RequestMetadataConsumerStatusResponse {
    state: String,
    pending_events: u64,
    lag_events: u64,
    oldest_pending_at: Option<DateTime<Utc>>,
    checked_at: Option<DateTime<Utc>>,
    heartbeat_age_seconds: Option<u64>,
}

impl From<RequestMetadataConsumerStatus> for RequestMetadataConsumerStatusResponse {
    fn from(consumer: RequestMetadataConsumerStatus) -> Self {
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
pub(in crate::operations) struct RequestMetadataGatewayEpochQuery {
    cursor: Option<String>,
    #[param(minimum = 1, maximum = 200)]
    limit: Option<u16>,
    /// Filter by open, gracefully_closed, unresolved, or acknowledged.
    state: Option<String>,
}

#[derive(Debug, Serialize, ToSchema)]
pub(in crate::operations) struct RequestMetadataGatewayEpochResponse {
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
}

impl From<RequestMetadataGatewayEpochRecord> for RequestMetadataGatewayEpochResponse {
    fn from(value: RequestMetadataGatewayEpochRecord) -> Self {
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
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(in crate::operations) struct RequestMetadataGatewayEpochListResponse {
    data: Vec<RequestMetadataGatewayEpochResponse>,
    next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/request-metadata/gateway-epochs",
    tag = "request-metadata",
    params(RequestMetadataGatewayEpochQuery),
    responses(
        (status = 200, description = "Request metadata gateway process epoch page", body = RequestMetadataGatewayEpochListResponse),
        (status = 400, description = "Invalid cursor or state filter", body = Problem)
    )
)]
pub(in crate::operations) async fn list_request_metadata_gateway_epochs(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Query(query): Query<RequestMetadataGatewayEpochQuery>,
) -> Result<Json<RequestMetadataGatewayEpochListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    let cursor = query
        .cursor
        .as_deref()
        .map(TimestampCursor::parse)
        .transpose()
        .map_err(map_operations)?;
    let state_filter = query
        .state
        .as_deref()
        .map(parse_request_metadata_gateway_epoch_state)
        .transpose()?;
    let page = state
        .store()
        .request_metadata_gateway_epochs(state_filter, cursor.as_ref(), page_limit(query.limit)?)
        .await
        .map_err(map_operations)?;
    Ok(Json(RequestMetadataGatewayEpochListResponse {
        data: page.items.into_iter().map(Into::into).collect(),
        next_cursor: page.next_cursor,
    }))
}

fn parse_request_metadata_gateway_epoch_state(
    value: &str,
) -> Result<RequestMetadataGatewayEpochState, Problem> {
    match value {
        "open" => Ok(RequestMetadataGatewayEpochState::Open),
        "gracefully_closed" => Ok(RequestMetadataGatewayEpochState::GracefullyClosed),
        "unresolved" => Ok(RequestMetadataGatewayEpochState::Unresolved),
        "acknowledged" => Ok(RequestMetadataGatewayEpochState::Acknowledged),
        _ => {
            let mut errors = FieldErrors::new();
            errors.insert(
                "state".to_owned(),
                vec!["Use open, gracefully_closed, unresolved, or acknowledged.".to_owned()],
            );
            Err(Problem::validation(errors))
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(in crate::operations) struct RequestMetadataEpochAcknowledgementResponse {
    #[schema(value_type = String, format = Uuid)]
    process_epoch: Uuid,
    gateway_instance: String,
    acknowledged_at: DateTime<Utc>,
    #[schema(value_type = Option<String>, format = Uuid)]
    acknowledged_by: Option<Uuid>,
}

impl From<RequestMetadataEpochAcknowledgement> for RequestMetadataEpochAcknowledgementResponse {
    fn from(value: RequestMetadataEpochAcknowledgement) -> Self {
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
    path = "/api/v1/request-metadata/gateway-epochs/{process_epoch}/acknowledge",
    tag = "request-metadata",
    params(("process_epoch" = Uuid, Path)),
    responses(
        (status = 200, description = "Unclean gateway epoch acknowledged; retained completeness evidence is unchanged", body = RequestMetadataEpochAcknowledgementResponse),
        (status = 404, description = "Unclean gateway epoch not found", body = Problem)
    )
)]
pub(in crate::operations) async fn acknowledge_request_metadata_gateway_epoch(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Path(process_epoch): Path<Uuid>,
) -> Result<Json<RequestMetadataEpochAcknowledgementResponse>, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageSettings)?;
    let acknowledgement = state
        .store()
        .acknowledge_request_metadata_gateway_epoch(process_epoch, principal.user_id)
        .await
        .map_err(map_persistence)?
        .ok_or_else(not_found)?;
    Ok(Json(acknowledgement.into()))
}
