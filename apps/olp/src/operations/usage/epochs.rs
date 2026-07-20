use axum::{
    Json,
    extract::{Path, Query, State},
    http::HeaderMap,
};
use chrono::{DateTime, Utc};
use olp_storage::{
    TimestampCursor, UsageEpochAcknowledgement, UsageGatewayEpochRecord, UsageGatewayEpochState,
};
use serde::{Deserialize, Serialize};
use utoipa::{IntoParams, ToSchema};
use uuid::Uuid;

use crate::{
    ApiState, FieldErrors, Problem,
    management::{
        Permission, map_persistence, require_mutation_session, require_permission,
        require_read_session, require_store,
    },
    operations::helpers::{map_operations, not_found, page_limit},
};

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub(in crate::operations) struct UsageGatewayEpochQuery {
    cursor: Option<String>,
    #[param(minimum = 1, maximum = 200)]
    limit: Option<u16>,
    /// Filter by open, gracefully_closed, unresolved, or acknowledged.
    state: Option<String>,
}

#[derive(Debug, Serialize, ToSchema)]
pub(in crate::operations) struct UsageGatewayEpochResponse {
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

impl From<UsageGatewayEpochRecord> for UsageGatewayEpochResponse {
    fn from(value: UsageGatewayEpochRecord) -> Self {
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
pub(in crate::operations) struct UsageGatewayEpochListResponse {
    data: Vec<UsageGatewayEpochResponse>,
    next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/usage/gateway-epochs",
    tag = "usage",
    params(UsageGatewayEpochQuery),
    responses(
        (status = 200, description = "Metadata-only gateway process epoch page", body = UsageGatewayEpochListResponse),
        (status = 400, description = "Invalid cursor or state filter", body = Problem)
    )
)]
pub(in crate::operations) async fn list_usage_gateway_epochs(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<UsageGatewayEpochQuery>,
) -> Result<Json<UsageGatewayEpochListResponse>, Problem> {
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
        .map(parse_usage_gateway_epoch_state)
        .transpose()?;
    let page = require_store(&state)?
        .usage_gateway_epochs(state_filter, cursor.as_ref(), page_limit(query.limit)?)
        .await
        .map_err(map_operations)?;
    Ok(Json(UsageGatewayEpochListResponse {
        data: page.items.into_iter().map(Into::into).collect(),
        next_cursor: page.next_cursor,
    }))
}

fn parse_usage_gateway_epoch_state(value: &str) -> Result<UsageGatewayEpochState, Problem> {
    match value {
        "open" => Ok(UsageGatewayEpochState::Open),
        "gracefully_closed" => Ok(UsageGatewayEpochState::GracefullyClosed),
        "unresolved" => Ok(UsageGatewayEpochState::Unresolved),
        "acknowledged" => Ok(UsageGatewayEpochState::Acknowledged),
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
pub(in crate::operations) struct UsageEpochAcknowledgementResponse {
    #[schema(value_type = String, format = Uuid)]
    process_epoch: Uuid,
    gateway_instance: String,
    acknowledged_at: DateTime<Utc>,
    #[schema(value_type = Option<String>, format = Uuid)]
    acknowledged_by: Option<Uuid>,
}

impl From<UsageEpochAcknowledgement> for UsageEpochAcknowledgementResponse {
    fn from(value: UsageEpochAcknowledgement) -> Self {
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
    path = "/api/v1/usage/gateway-epochs/{process_epoch}/acknowledge",
    tag = "usage",
    params(("process_epoch" = Uuid, Path)),
    responses(
        (status = 200, description = "Unclean gateway epoch acknowledged; retained completeness evidence is unchanged", body = UsageEpochAcknowledgementResponse),
        (status = 404, description = "Unclean gateway epoch not found", body = Problem)
    )
)]
pub(in crate::operations) async fn acknowledge_usage_gateway_epoch(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Path(process_epoch): Path<Uuid>,
) -> Result<Json<UsageEpochAcknowledgementResponse>, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageSettings)?;
    let acknowledgement = require_store(&state)?
        .acknowledge_usage_gateway_epoch(process_epoch, principal.user_id)
        .await
        .map_err(map_persistence)?
        .ok_or_else(not_found)?;
    Ok(Json(acknowledgement.into()))
}
