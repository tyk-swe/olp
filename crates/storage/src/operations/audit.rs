use chrono::{DateTime, Utc};
use sqlx::{Postgres, QueryBuilder, Row};
use uuid::Uuid;

use super::{
    MAX_PAGE_SIZE,
    cursor::{OperationsError, Page, TimestampCursor},
};
use crate::{PgStore, split_page};

#[derive(Clone, Debug)]
pub struct AuditRecord {
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

impl PgStore {
    pub async fn audit_events(
        &self,
        cursor: Option<&TimestampCursor>,
        limit: u16,
    ) -> Result<Page<AuditRecord>, OperationsError> {
        let page_size = limit.clamp(1, MAX_PAGE_SIZE);
        let mut query = QueryBuilder::<Postgres>::new(
            "SELECT a.id, a.actor_user_id, u.email AS actor_email, a.action, a.resource_type, \
                    a.resource_id, a.outcome, a.source_ip::text AS source_ip, \
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
        query.push(" ORDER BY a.occurred_at DESC, a.id DESC LIMIT ");
        query.push_bind(i64::from(page_size) + 1);
        let rows = query.build().fetch_all(self.pool()).await?;
        let items = rows
            .into_iter()
            .map(|row| AuditRecord {
                id: row.get("id"),
                actor_user_id: row.get("actor_user_id"),
                actor_email: row.get("actor_email"),
                action: row.get("action"),
                resource_type: row.get("resource_type"),
                resource_id: row.get("resource_id"),
                outcome: row.get("outcome"),
                source_ip: row.get("source_ip"),
                user_agent_family: row.get("user_agent_family"),
                occurred_at: row.get("occurred_at"),
            })
            .collect::<Vec<_>>();
        let (items, next_cursor) = split_page(items, usize::from(page_size), |item| {
            TimestampCursor {
                at: item.occurred_at,
                id: item.id,
            }
            .encode()
        });
        Ok(Page { items, next_cursor })
    }
}
