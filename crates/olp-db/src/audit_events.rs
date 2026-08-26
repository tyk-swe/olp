use chrono::{DateTime, Utc};
use sqlx::{Executor, Postgres};
use uuid::Uuid;

use crate::store::RequestProvenance;

/// Every column an `audit_events` row carries. `occurred_at` left as `None`
/// falls back to the database clock, matching the column default.
pub(crate) struct AuditEvent<'a> {
    pub(crate) provenance: &'a RequestProvenance,
    pub(crate) actor: Option<Uuid>,
    pub(crate) action: &'a str,
    pub(crate) resource_type: &'a str,
    pub(crate) resource_id: Option<&'a str>,
    pub(crate) outcome: &'a str,
    pub(crate) occurred_at: Option<DateTime<Utc>>,
}

pub(crate) async fn record_audit_event<'e, E>(
    executor: E,
    event: AuditEvent<'_>,
) -> Result<(), sqlx::Error>
where
    E: Executor<'e, Database = Postgres>,
{
    sqlx::query!(
        "INSERT INTO audit_events \
         (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at, \
          source_ip, user_agent_family) \
         VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7::timestamptz, now()), $8::text::inet, $9)",
        Uuid::now_v7(),
        event.actor,
        event.action,
        event.resource_type,
        event.resource_id,
        event.outcome,
        event.occurred_at,
        event.provenance.source_ip_text(),
        event.provenance.user_agent_family()
    )
    .execute(executor)
    .await?;
    Ok(())
}

/// The shape almost every writer in this crate needs: a successful action by a
/// known actor against a single UUID-keyed resource, timestamped by the
/// database clock. Use [`record_audit_event`] directly for anything else --- a
/// failure outcome, an anonymous actor, or a resource id that is not a UUID.
pub(crate) async fn record_success<'e, E>(
    executor: E,
    provenance: &RequestProvenance,
    actor: Uuid,
    action: &str,
    resource_type: &str,
    resource_id: Uuid,
) -> Result<(), sqlx::Error>
where
    E: Executor<'e, Database = Postgres>,
{
    record_success_inner(
        executor,
        provenance,
        actor,
        action,
        resource_type,
        resource_id,
        None,
    )
    .await
}

/// [`record_success`] for callers that already hold the transaction's clock and
/// want every row it writes stamped with the same instant.
pub(crate) async fn record_success_at<'e, E>(
    executor: E,
    provenance: &RequestProvenance,
    actor: Uuid,
    action: &str,
    resource_type: &str,
    resource_id: Uuid,
    occurred_at: DateTime<Utc>,
) -> Result<(), sqlx::Error>
where
    E: Executor<'e, Database = Postgres>,
{
    record_success_inner(
        executor,
        provenance,
        actor,
        action,
        resource_type,
        resource_id,
        Some(occurred_at),
    )
    .await
}

async fn record_success_inner<'e, E>(
    executor: E,
    provenance: &RequestProvenance,
    actor: Uuid,
    action: &str,
    resource_type: &str,
    resource_id: Uuid,
    occurred_at: Option<DateTime<Utc>>,
) -> Result<(), sqlx::Error>
where
    E: Executor<'e, Database = Postgres>,
{
    record_audit_event(
        executor,
        AuditEvent {
            provenance,
            actor: Some(actor),
            action,
            resource_type,
            resource_id: Some(&resource_id.to_string()),
            outcome: "success",
            occurred_at,
        },
    )
    .await
}
