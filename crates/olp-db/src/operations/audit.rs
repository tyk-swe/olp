use chrono::{DateTime, Utc};
use sqlx::{FromRow, Postgres, QueryBuilder};
use uuid::Uuid;

use super::{
    MAX_PAGE_SIZE,
    cursor::{Error, Page, Timestamp},
};
use crate::{split_page, store::Store};

#[derive(Clone, Debug, FromRow)]
pub struct Record {
    pub id: Uuid,
    pub actor_user_id: Option<Uuid>,
    pub actor_email: Option<String>,
    pub action: String,
    pub resource_type: String,
    pub resource_id: Option<String>,
    pub outcome: String,
    pub source_ip: Option<String>,
    pub user_agent_family: Option<String>,
    pub occurred_at: DateTime<Utc>,
}

/// Equality filters applied to one audit page. Every populated field narrows
/// the page further; an empty value set returns the whole stream.
#[derive(Clone, Debug, Default)]
pub struct Filters {
    pub action: Option<String>,
    pub resource_type: Option<String>,
    pub resource_id: Option<String>,
    pub actor_user_id: Option<Uuid>,
    pub outcome: Option<String>,
    pub occurred_after: Option<DateTime<Utc>>,
    pub occurred_before: Option<DateTime<Utc>>,
}

impl Store {
    pub async fn audit_events(
        &self,
        cursor: Option<&Timestamp>,
        limit: u16,
        filters: &Filters,
    ) -> Result<Page<Record>, Error> {
        let page_size = limit.clamp(1, MAX_PAGE_SIZE);
        let mut query = QueryBuilder::<Postgres>::new(
            "SELECT a.id, a.actor_user_id, u.email AS actor_email, a.action, a.resource_type, \
                    a.resource_id, a.outcome, host(a.source_ip) AS source_ip, \
                    a.user_agent_family, a.occurred_at \
             FROM audit_events a LEFT JOIN users u ON u.id = a.actor_user_id WHERE true",
        );
        if let Some(cursor) = cursor {
            query.push(" AND (a.occurred_at, a.id) < (");
            query.push_bind(cursor.at);
            query.push(", ");
            query.push_bind(cursor.id);
            query.push(")");
        }
        if let Some(action) = filters.action.as_deref() {
            query.push(" AND a.action = ");
            query.push_bind(action.to_owned());
        }
        if let Some(resource_type) = filters.resource_type.as_deref() {
            query.push(" AND a.resource_type = ");
            query.push_bind(resource_type.to_owned());
        }
        if let Some(resource_id) = filters.resource_id.as_deref() {
            query.push(" AND a.resource_id = ");
            query.push_bind(resource_id.to_owned());
        }
        if let Some(actor_user_id) = filters.actor_user_id {
            query.push(" AND a.actor_user_id = ");
            query.push_bind(actor_user_id);
        }
        if let Some(outcome) = filters.outcome.as_deref() {
            query.push(" AND a.outcome = ");
            query.push_bind(outcome.to_owned());
        }
        if let Some(occurred_after) = filters.occurred_after {
            query.push(" AND a.occurred_at >= ");
            query.push_bind(occurred_after);
        }
        if let Some(occurred_before) = filters.occurred_before {
            query.push(" AND a.occurred_at <= ");
            query.push_bind(occurred_before);
        }
        query.push(" ORDER BY a.occurred_at DESC, a.id DESC LIMIT ");
        query.push_bind(i64::from(page_size) + 1);
        let items = query
            .build_query_as::<Record>()
            .fetch_all(self.pool())
            .await?;
        let (items, next_cursor) = split_page(items, usize::from(page_size), |item| {
            Timestamp {
                at: item.occurred_at,
                id: item.id,
            }
            .encode()
        });
        Ok(Page { items, next_cursor })
    }
}
