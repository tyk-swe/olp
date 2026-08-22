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

impl Store {
    pub async fn audit_events(
        &self,
        cursor: Option<&Timestamp>,
        limit: u16,
    ) -> Result<Page<Record>, Error> {
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
