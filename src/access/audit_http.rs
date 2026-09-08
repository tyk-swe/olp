use crate::access::audit::Filters;
use crate::access::audit::Record;
use crate::access::policy::Permission;
use crate::access::principal::ReadPrincipal;
use axum::Json;
use axum::extract::Query;
use axum::extract::State;
use axum::extract::rejection::QueryRejection;
use chrono::DateTime;
use chrono::Utc;
use serde::Deserialize;
use serde::Serialize;
use utoipa::IntoParams;
use utoipa::ToSchema;
use uuid::Uuid;

use crate::access::permissions::require_permission;
use crate::http::control::operations::helpers::map_operations;
use crate::http::control::operations::helpers::optional_filter;
use crate::http::control::operations::helpers::query_parameters;
use crate::http::control::operations::helpers::timestamp_cursor;
use crate::http::control::operations::helpers::validate_time_range;
use crate::http::control::pagination::page_limit;
use crate::http::control::state::ManagementState;
use crate::http::problem::Problem;

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct AuditEventResponse {
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
pub(crate) struct AuditQuery {
    /// Opaque cursor returned by the previous page.
    pub(crate) cursor: Option<String>,
    /// Page size, from 1 to 200. Defaults to 50.
    #[param(minimum = 1, maximum = 200)]
    pub(crate) limit: Option<u16>,
    /// Exact audit action, such as `provider.update`.
    pub(crate) action: Option<String>,
    /// Exact resource type, such as `provider`.
    pub(crate) resource_type: Option<String>,
    /// Exact resource identifier, as recorded on the event.
    pub(crate) resource_id: Option<String>,
    /// Identifier of the acting user.
    #[param(value_type = Option<String>, format = Uuid)]
    pub(crate) actor_user_id: Option<Uuid>,
    /// Exact outcome, `success` or `failure`.
    pub(crate) outcome: Option<String>,
    /// Oldest event to return, inclusive.
    pub(crate) occurred_after: Option<DateTime<Utc>>,
    /// Newest event to return, inclusive.
    pub(crate) occurred_before: Option<DateTime<Utc>>,
}

impl AuditQuery {
    fn filters(&self) -> Result<Filters, Problem> {
        if let (Some(after), Some(before)) = (self.occurred_after, self.occurred_before) {
            validate_time_range("occurred_after", after, "occurred_before", before)?;
        }
        Ok(Filters {
            action: optional_filter(self.action.as_ref()),
            resource_type: optional_filter(self.resource_type.as_ref()),
            resource_id: optional_filter(self.resource_id.as_ref()),
            actor_user_id: self.actor_user_id,
            outcome: optional_filter(self.outcome.as_ref()),
            occurred_after: self.occurred_after,
            occurred_before: self.occurred_before,
        })
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct AuditListResponse {
    items: Vec<AuditEventResponse>,
    next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v3/audit",
    tag = "audit",
    params(AuditQuery),
    responses(
        (status = 200, description = "Audit page", body = AuditListResponse),
        (status = 400, description = "Malformed query parameters, or an invalid cursor or page size", body = Problem, content_type = "application/problem+json"),
        (status = 422, description = "Invalid time range", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [])))]
pub(crate) async fn list_audit_events(
    State(state): State<ManagementState>,
    query: Result<Query<AuditQuery>, QueryRejection>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<AuditListResponse>, Problem> {
    require_permission(&principal, Permission::ReadOperations)?;
    let query = query_parameters(query)?;
    let cursor = timestamp_cursor(query.cursor.as_deref())?;
    let limit = page_limit(query.limit)?;
    let filters = query.filters()?;
    let page = crate::access::audit::audit_events(
        &state.request_boundary.pool,
        cursor.as_ref(),
        limit,
        &filters,
    )
    .await
    .map_err(map_operations)?;
    let items = page.items.into_iter().map(Into::into).collect::<Vec<_>>();
    Ok(Json(AuditListResponse {
        items,
        next_cursor: page.next_cursor,
    }))
}

pub(crate) fn router()
-> utoipa_axum::router::OpenApiRouter<crate::http::control::state::ManagementState> {
    utoipa_axum::router::OpenApiRouter::new().routes(utoipa_axum::routes!(
        crate::access::audit_http::list_audit_events
    ))
}

#[cfg(test)]
mod tests {
    use axum::extract::Query;
    use chrono::Duration;
    use chrono::Utc;

    use crate::access::audit_http::AuditQuery;
    use crate::access::audit_http::query_parameters;

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
    fn an_inverted_time_range_fails_field_validation() {
        let now = Utc::now();
        let problem = AuditQuery {
            occurred_after: Some(now),
            occurred_before: Some(now - Duration::seconds(1)),
            ..query()
        }
        .filters()
        .unwrap_err();
        assert_eq!(problem.status, 422);
        assert_eq!(
            problem.errors.get("occurred_before").map(|errors| errors
                .iter()
                .map(|error| error.message.clone())
                .collect::<Vec<_>>()),
            Some(vec![
                "occurred_before must be later than occurred_after.".to_owned()
            ])
        );
    }

    #[test]
    fn a_single_instant_range_is_rejected_like_every_other_collection() {
        let now = Utc::now();
        let problem = AuditQuery {
            occurred_after: Some(now),
            occurred_before: Some(now),
            ..query()
        }
        .filters()
        .unwrap_err();
        assert_eq!(problem.status, 422);
    }

    #[test]
    fn blank_string_filters_are_treated_as_absent() {
        let filters = AuditQuery {
            action: Some(String::new()),
            resource_type: Some("   ".to_owned()),
            resource_id: Some("\t\n".to_owned()),
            outcome: Some("  success  ".to_owned()),
            ..query()
        }
        .filters()
        .unwrap();
        assert!(filters.action.is_none());
        assert!(filters.resource_type.is_none());
        assert!(filters.resource_id.is_none());
        assert_eq!(filters.outcome.as_deref(), Some("success"));
    }

    #[test]
    fn a_malformed_query_parameter_becomes_a_problem() {
        let rejection = Query::<AuditQuery>::try_from_uri(
            &"http://olp.test/api/v3/audit?actor_user_id=not-a-uuid"
                .parse()
                .unwrap(),
        )
        .unwrap_err();
        let problem = query_parameters::<AuditQuery>(Err(rejection)).unwrap_err();
        assert_eq!(problem.status, 400);
        assert_eq!(
            problem.problem_type.as_ref(),
            "https://openllmproxy.dev/problems/invalid_query_parameters"
        );
        assert!(!problem.detail.contains("not-a-uuid"));
    }
}
