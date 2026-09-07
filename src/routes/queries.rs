use crate::protocols::canonical::identity::OperationKind;
use uuid::Uuid;
/// Column arrays for one multi-row `route_draft_targets` insert. Rows are
/// pushed in position order and `UNNEST` zips the arrays back into rows, so a
/// draft with N targets costs one statement instead of N.
#[derive(Default)]
pub(crate) struct DraftTargetRows {
    ids: Vec<Uuid>,
    routing_ids: Vec<Uuid>,
    provider_model_ids: Vec<Uuid>,
    priorities: Vec<i32>,
    weights: Vec<i32>,
    timeouts_ms: Vec<i32>,
    positions: Vec<i32>,
}

impl DraftTargetRows {
    pub(crate) fn push(
        &mut self,
        routing_id: Uuid,
        provider_model_id: Uuid,
        priority: i32,
        weight: i32,
        timeout_ms: i32,
        position: i32,
    ) {
        self.ids.push(Uuid::now_v7());
        self.routing_ids.push(routing_id);
        self.provider_model_ids.push(provider_model_id);
        self.priorities.push(priority);
        self.weights.push(weight);
        self.timeouts_ms.push(timeout_ms);
        self.positions.push(position);
    }
}

pub(crate) async fn insert_draft_targets(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    draft_id: Uuid,
    rows: &DraftTargetRows,
) -> Result<(), sqlx::Error> {
    sqlx::query(
        "INSERT INTO route_draft_targets \
         (id, routing_id, route_draft_id, provider_model_id, priority, weight, timeout_ms, position) \
         SELECT t.id, t.routing_id, $1, t.provider_model_id, t.priority, t.weight, t.timeout_ms, t.position \
         FROM UNNEST($2::uuid[], $3::uuid[], $4::uuid[], $5::int4[], $6::int4[], $7::int4[], $8::int4[]) \
           AS t(id, routing_id, provider_model_id, priority, weight, timeout_ms, position)",
    )
    .bind(draft_id)
    .bind(&rows.ids)
    .bind(&rows.routing_ids)
    .bind(&rows.provider_model_ids)
    .bind(&rows.priorities)
    .bind(&rows.weights)
    .bind(&rows.timeouts_ms)
    .bind(&rows.positions)
    .execute(&mut **transaction)
    .await?;
    Ok(())
}

/// Inserts a draft's operation set in one statement.
pub(crate) async fn insert_draft_operations(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    draft_id: Uuid,
    operations: &[OperationKind],
) -> Result<(), sqlx::Error> {
    let operations = operations
        .iter()
        .map(|operation| operation.as_str().to_owned())
        .collect::<Vec<_>>();
    sqlx::query(
        "INSERT INTO route_draft_operations (route_draft_id, operation) \
         SELECT $1, UNNEST($2::text[])",
    )
    .bind(draft_id)
    .bind(&operations)
    .execute(&mut **transaction)
    .await?;
    Ok(())
}
