use axum::{
    Json,
    extract::{Query, State},
    http::HeaderMap,
};
use chrono::{DateTime, Utc};
use olp_db::operations::audit::{Filters, Record};
use olp_engine::domain::auth::Permission;
use serde::{Deserialize, Serialize};
use utoipa::{IntoParams, ToSchema};
use uuid::Uuid;

use super::helpers::{map_operations, page_limit, timestamp_cursor};
use crate::{
    bootstrap::mode_dependencies::ManagementState,
    management::{permissions::require_permission, sessions::require_read_session},
    public_http::problem::Problem,
};

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct AuditEventResponse {
    #[schema(value_type = String, format = Uuid)]
    id: Uuid,
    #[schema(value_type = Option<String>, format = Uuid)]
    actor_user_id: Option<Uuid>,
    actor_email: Option<String>,
    action: String,
    resource_type: String,
    resource_id: Option<String>,
    outcome: String,
    /// Source address recorded for the request, when the boundary supplied one.
    source_ip: Option<String>,
    /// Coarse user-agent family; the full user-agent string is never stored.
    user_agent_family: Option<String>,
    occurred_at: DateTime<Utc>,
}

impl From<Record> for AuditEventResponse {
    fn from(record: Record) -> Self {
        Self {
            id: record.id,
            actor_user_id: record.actor_user_id,
            actor_email: record.actor_email,
            action: record.action,
            resource_type: record.resource_type,
            resource_id: record.resource_id,
            outcome: record.outcome,
            source_ip: record.source_ip,
            user_agent_family: record.user_agent_family,
            occurred_at: record.occurred_at,
        }
    }
}

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub(super) struct AuditQuery {
    /// Opaque cursor returned by the previous page.
    pub(super) cursor: Option<String>,
    /// Page size, from 1 to 200. Defaults to 50.
    #[param(minimum = 1, maximum = 200)]
    pub(super) limit: Option<u16>,
    /// Exact audit action, such as `provider.update`.
    pub(super) action: Option<String>,
    /// Exact resource type, such as `provider`.
    pub(super) resource_type: Option<String>,
    /// Exact resource identifier, as recorded on the event.
    pub(super) resource_id: Option<String>,
    /// Identifier of the acting user.
    #[param(value_type = Option<String>, format = Uuid)]
    pub(super) actor_user_id: Option<Uuid>,
    /// Exact outcome, `success` or `failure`.
    pub(super) outcome: Option<String>,
    /// Oldest event to return, inclusive.
    pub(super) occurred_after: Option<DateTime<Utc>>,
    /// Newest event to return, inclusive.
    pub(super) occurred_before: Option<DateTime<Utc>>,
}

impl AuditQuery {
    fn filters(&self) -> Result<Filters, Problem> {
        if let (Some(after), Some(before)) = (self.occurred_after, self.occurred_before)
            && after > before
        {
            return Err(Problem::bad_request(
                "invalid_time_range",
                "occurred_after must not be later than occurred_before.",
            ));
        }
        Ok(Filters {
            action: self.action.clone(),
            resource_type: self.resource_type.clone(),
            resource_id: self.resource_id.clone(),
            actor_user_id: self.actor_user_id,
            outcome: self.outcome.clone(),
            occurred_after: self.occurred_after,
            occurred_before: self.occurred_before,
        })
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct AuditListResponse {
    data: Vec<AuditEventResponse>,
    next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/audit",
    tag = "audit",
    params(AuditQuery),
    responses(
        (status = 200, description = "Audit page", body = AuditListResponse),
        (status = 400, description = "Invalid cursor, page size, or time range", body = Problem)
    )
)]
pub(super) async fn list_audit_events(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Query(query): Query<AuditQuery>,
) -> Result<Json<AuditListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    let cursor = timestamp_cursor(query.cursor.as_deref())?;
    let limit = page_limit(query.limit)?;
    let filters = query.filters()?;
    let page = state
        .store()
        .audit_events(cursor.as_ref(), limit, &filters)
        .await
        .map_err(map_operations)?;
    Ok(Json(AuditListResponse {
        data: page.items.into_iter().map(Into::into).collect(),
        next_cursor: page.next_cursor,
    }))
}

#[cfg(test)]
mod tests {
    use chrono::{Duration, Utc};

    use super::AuditQuery;

    fn query() -> AuditQuery {
        AuditQuery {
            cursor: None,
            limit: None,
            action: None,
            resource_type: None,
            resource_id: None,
            actor_user_id: None,
            outcome: None,
            occurred_after: None,
            occurred_before: None,
        }
    }

    #[test]
    fn an_empty_query_filters_nothing() {
        let filters = query().filters().unwrap();
        assert!(filters.action.is_none());
        assert!(filters.actor_user_id.is_none());
        assert!(filters.occurred_after.is_none());
        assert!(filters.occurred_before.is_none());
    }

    #[test]
    fn every_supplied_value_reaches_the_store_filters() {
        let actor = uuid::Uuid::now_v7();
        let after = Utc::now() - Duration::hours(1);
        let before = Utc::now();
        let filters = AuditQuery {
            action: Some("provider.update".to_owned()),
            resource_type: Some("provider".to_owned()),
            resource_id: Some("provider-1".to_owned()),
            actor_user_id: Some(actor),
            outcome: Some("success".to_owned()),
            occurred_after: Some(after),
            occurred_before: Some(before),
            ..query()
        }
        .filters()
        .unwrap();
        assert_eq!(filters.action.as_deref(), Some("provider.update"));
        assert_eq!(filters.resource_type.as_deref(), Some("provider"));
        assert_eq!(filters.resource_id.as_deref(), Some("provider-1"));
        assert_eq!(filters.actor_user_id, Some(actor));
        assert_eq!(filters.outcome.as_deref(), Some("success"));
        assert_eq!(filters.occurred_after, Some(after));
        assert_eq!(filters.occurred_before, Some(before));
    }

    #[test]
    fn an_inverted_time_range_is_rejected_as_a_bad_request() {
        let now = Utc::now();
        let problem = AuditQuery {
            occurred_after: Some(now),
            occurred_before: Some(now - Duration::seconds(1)),
            ..query()
        }
        .filters()
        .unwrap_err();
        assert_eq!(problem.status, 400);
        assert_eq!(
            problem.detail.as_ref(),
            "occurred_after must not be later than occurred_before."
        );
    }

    #[test]
    fn a_single_instant_range_is_accepted() {
        let now = Utc::now();
        let filters = AuditQuery {
            occurred_after: Some(now),
            occurred_before: Some(now),
            ..query()
        }
        .filters()
        .unwrap();
        assert_eq!(filters.occurred_after, filters.occurred_before);
    }
}
