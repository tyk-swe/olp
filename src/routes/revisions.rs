use crate::access::audit_events::record_success;
use crate::database::error::Error as PersistenceError;
use crate::database::idempotency::claim_idempotency;
use crate::database::idempotency::complete_idempotency;
use crate::database::page::ConfigurationPage;
use crate::database::query::split_page;
use crate::protocols::canonical::identity::OperationKind;
use crate::providers::error::Error;
use crate::providers::record_validation::checked_limit;
use crate::routes::records::RouteDraftRecord;
use crate::routes::records::RouteRevisionDiff;
use crate::routes::records::RouteRevisionRecord;
use crate::routes::records::RouteTargetRecord;
use crate::routes::repository::RouteTargetRow;
use std::collections::BTreeMap;
use std::collections::BTreeSet;
use uuid::Uuid;

pub async fn list_route_revisions(
    pool: &sqlx::PgPool,
    route_id: Uuid,
    cursor: Option<Uuid>,
    limit: i64,
) -> Result<ConfigurationPage<RouteRevisionRecord>, Error> {
    let limit = checked_limit(limit)?;
    let exists: bool = sqlx::query_scalar::<_, bool>(
        "SELECT EXISTS (SELECT 1 FROM routes WHERE id = $1) AS \"value\"",
    )
    .bind(route_id)
    .fetch_one(pool)
    .await?;
    if !exists {
        return Err(Error::NotFound);
    }
    let before_revision: Option<i32> = match cursor {
        Some(cursor) => Some(
            sqlx::query_scalar::<_, i32>(
                "SELECT revision FROM route_revisions WHERE route_id = $1 AND id = $2",
            )
            .bind(route_id)
            .bind(cursor)
            .fetch_optional(pool)
            .await?
            .ok_or_else(|| {
                Error::Invalid("route-revision pagination cursor is invalid".to_owned())
            })?,
        ),
        None => None,
    };
    let ids: Vec<Uuid> = sqlx::query_scalar::<_, uuid::Uuid>(
        "SELECT id FROM route_revisions WHERE route_id = $1 \
             AND ($2::int IS NULL OR revision < $2) \
             ORDER BY revision DESC LIMIT $3",
    )
    .bind(route_id)
    .bind(before_revision)
    .bind(limit + 1)
    .fetch_all(pool)
    .await?;
    let (ids, next_cursor) = split_page(ids, limit as usize, |id| *id);
    let mut revisions_map = crate::routes::revisions::get_route_revisions(pool, &ids).await?;
    let mut revisions = Vec::with_capacity(ids.len());
    for id in ids {
        let rev = revisions_map.remove(&id).ok_or(Error::NotFound)?;
        revisions.push(rev);
    }
    Ok(ConfigurationPage {
        items: revisions,
        next_cursor,
    })
}

pub async fn get_route_revisions(
    pool: &sqlx::PgPool,
    revision_ids: &[Uuid],
) -> Result<BTreeMap<Uuid, RouteRevisionRecord>, Error> {
    if revision_ids.is_empty() {
        return Ok(BTreeMap::new());
    }

    let revision_rows = sqlx::query_as::<_, GetRouteRevisionsRow>(
        "SELECT id, routing_id, route_id, revision, slug, overall_timeout_ms, max_attempts, source_draft_id, \
                    activated_by, activated_at FROM route_revisions WHERE id = ANY($1::uuid[])",
    )
    .bind(revision_ids)
        .fetch_all(pool)
        .await?;

    let operation_rows = sqlx::query_as::<_, RouteRevisionOperationRow>(
        "SELECT route_revision_id, operation FROM route_revision_operations \
             WHERE route_revision_id = ANY($1::uuid[]) ORDER BY route_revision_id, operation",
    )
    .bind(revision_ids)
    .fetch_all(pool)
    .await?;

    let mut operations_map = BTreeMap::<Uuid, Vec<OperationKind>>::new();
    for row in operation_rows {
        let op = row
            .operation
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("route revision operation"))?;
        operations_map
            .entry(row.route_revision_id)
            .or_default()
            .push(op);
    }

    let target_rows_raw = sqlx::query_as::<_, RouteRevisionTargetRow>(
        "SELECT rrt.route_revision_id, rrt.id, rrt.routing_id, rrt.provider_model_id, \
                    p.id AS provider_id, COALESCE(pr.name, p.name) AS \"provider_name\", \
                    COALESCE(prm.upstream_model, pm.upstream_model) AS \"provider_model\", \
                    (p.state <> 'disabled'::provider_state AND prm.id IS NOT NULL \
                     AND COALESCE(prm.enabled, false)) AS \"available\", \
                    rrt.priority, rrt.weight, rrt.timeout_ms, rrt.position \
             FROM route_revision_targets rrt \
             JOIN provider_models pm ON pm.id = rrt.provider_model_id \
             JOIN providers p ON p.id = pm.provider_id \
             LEFT JOIN provider_revisions pr ON pr.id = p.active_revision_id \
             LEFT JOIN provider_revision_models prm ON prm.provider_revision_id = pr.id \
               AND prm.source_provider_model_id = pm.id \
             WHERE rrt.route_revision_id = ANY($1::uuid[]) ORDER BY rrt.route_revision_id, rrt.position",
    )
    .bind(revision_ids)
        .fetch_all(pool)
        .await?;

    let mut targets_map = BTreeMap::<Uuid, Vec<RouteTargetRecord>>::new();
    for row in target_rows_raw {
        let (revision_id, target) = row.split();
        targets_map.entry(revision_id).or_default().push(target);
    }

    let mut revisions = BTreeMap::new();
    for row in revision_rows {
        let rev_id = row.id;
        revisions.insert(
            rev_id,
            RouteRevisionRecord {
                id: rev_id,
                routing_id: row.routing_id,
                route_id: row.route_id,
                revision: row.revision,
                slug: row.slug,
                overall_timeout_ms: row.overall_timeout_ms,
                max_attempts: row.max_attempts,
                source_draft_id: row.source_draft_id,
                activated_by: row.activated_by,
                activated_at: row.activated_at,
                operations: operations_map.remove(&rev_id).unwrap_or_default(),
                targets: targets_map.remove(&rev_id).unwrap_or_default(),
            },
        );
    }
    Ok(revisions)
}

pub async fn get_route_revision(
    pool: &sqlx::PgPool,
    route_id: Uuid,
    revision_id: Uuid,
) -> Result<RouteRevisionRecord, Error> {
    let rev = crate::routes::revisions::get_route_revisions(pool, &[revision_id])
        .await?
        .remove(&revision_id)
        .ok_or(Error::NotFound)?;
    if rev.route_id != route_id {
        return Err(Error::NotFound);
    }
    Ok(rev)
}

pub async fn diff_route_revisions(
    pool: &sqlx::PgPool,
    route_id: Uuid,
    from_id: Uuid,
    to_id: Uuid,
) -> Result<RouteRevisionDiff, Error> {
    let from = crate::routes::revisions::get_route_revision(pool, route_id, from_id).await?;
    let to = crate::routes::revisions::get_route_revision(pool, route_id, to_id).await?;
    let from_operations: BTreeSet<_> = from.operations.iter().cloned().collect();
    let to_operations: BTreeSet<_> = to.operations.iter().cloned().collect();
    let from_targets = revision_target_map(&from.targets);
    let to_targets = revision_target_map(&to.targets);
    Ok(RouteRevisionDiff {
        from_revision: from.revision,
        to_revision: to.revision,
        slug_changed: from.slug != to.slug,
        timeout_changed: from.overall_timeout_ms != to.overall_timeout_ms,
        max_attempts_changed: from.max_attempts != to.max_attempts,
        operations_added: to_operations
            .difference(&from_operations)
            .copied()
            .collect(),
        operations_removed: from_operations
            .difference(&to_operations)
            .copied()
            .collect(),
        targets_added: to_targets
            .keys()
            .filter(|key| !from_targets.contains_key(*key))
            .cloned()
            .collect(),
        targets_removed: from_targets
            .keys()
            .filter(|key| !to_targets.contains_key(*key))
            .cloned()
            .collect(),
        targets_changed: to_targets
            .iter()
            .filter_map(|(key, value)| {
                from_targets
                    .get(key)
                    .filter(|old| *old != value)
                    .map(|_| key.clone())
            })
            .collect(),
    })
}

pub async fn restore_route_revision_as_draft(
    pool: &sqlx::PgPool,
    provenance: &crate::database::RequestProvenance,
    route_id: Uuid,
    revision_id: Uuid,
    actor: Uuid,
    idempotency_key: &str,
) -> Result<RouteDraftRecord, Error> {
    let revision =
        crate::routes::revisions::get_route_revision(pool, route_id, revision_id).await?;
    let mut transaction = pool.begin().await?;
    if !claim_idempotency(
        &mut transaction,
        actor,
        "route.restore_as_draft",
        idempotency_key,
    )
    .await?
    {
        return Err(Error::IdempotencyConflict);
    }
    let id = Uuid::now_v7();
    let etag = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO route_drafts \
             (id, routing_id, slug, state, overall_timeout_ms, max_attempts, etag, based_on_revision_id, created_by) \
             VALUES ($1, $2, $3, 'draft'::route_draft_state, $4, $5, $6, $7, $8)",
    )
    .bind(id)
    .bind(revision.routing_id)
    .bind(&revision.slug)
    .bind(revision.overall_timeout_ms)
    .bind(revision.max_attempts)
    .bind(etag)
    .bind(revision_id)
    .bind(actor)
        .execute(&mut *transaction)
        .await?;
    sqlx::query(
        "INSERT INTO route_draft_operations (route_draft_id, operation) \
             SELECT $1, operation FROM route_revision_operations WHERE route_revision_id = $2",
    )
    .bind(id)
    .bind(revision_id)
    .execute(&mut *transaction)
    .await?;
    sqlx::query(
        "INSERT INTO route_draft_targets \
             (id, routing_id, route_draft_id, provider_model_id, priority, weight, timeout_ms, position) \
             SELECT uuidv7(), routing_id, $1, provider_model_id, priority, weight, timeout_ms, position \
             FROM route_revision_targets WHERE route_revision_id = $2",
    )
    .bind(id)
    .bind(revision_id)
        .execute(&mut *transaction)
        .await?;
    record_success(
        &mut *transaction,
        provenance,
        actor,
        "route.restore_as_draft",
        "route_draft",
        id,
    )
    .await?;
    complete_idempotency(
        &mut transaction,
        actor,
        "route.restore_as_draft",
        idempotency_key,
        &id.to_string(),
    )
    .await?;
    transaction.commit().await?;
    crate::routes::repository::get_route_draft(pool, id).await
}

#[derive(Debug, sqlx::FromRow)]
struct RouteRevisionTargetRow {
    route_revision_id: Uuid,
    id: Uuid,
    routing_id: Uuid,
    provider_model_id: Uuid,
    provider_id: Uuid,
    provider_name: String,
    provider_model: String,
    available: bool,
    priority: i32,
    weight: i32,
    timeout_ms: i32,
    position: i32,
}

impl RouteRevisionTargetRow {
    fn split(self) -> (Uuid, RouteTargetRecord) {
        let Self {
            route_revision_id,
            id,
            routing_id,
            provider_model_id,
            provider_id,
            provider_name,
            provider_model,
            available,
            priority,
            weight,
            timeout_ms,
            position,
        } = self;
        let target = RouteTargetRow {
            id,
            routing_id,
            provider_model_id,
            provider_id,
            provider_name,
            provider_model,
            available,
            priority,
            weight,
            timeout_ms,
            position,
        };
        (route_revision_id, target.into())
    }
}

fn revision_target_map(targets: &[RouteTargetRecord]) -> BTreeMap<String, (i32, i32, i32, i32)> {
    targets
        .iter()
        .map(|target| {
            (
                format!("{}/{}", target.provider_id, target.upstream_model),
                (
                    target.priority,
                    target.weight,
                    target.timeout_ms,
                    target.position,
                ),
            )
        })
        .collect()
}

#[derive(sqlx::FromRow)]
struct GetRouteRevisionsRow {
    id: uuid::Uuid,
    routing_id: uuid::Uuid,
    route_id: uuid::Uuid,
    revision: i32,
    slug: String,
    overall_timeout_ms: i32,
    max_attempts: i16,
    source_draft_id: uuid::Uuid,
    activated_by: uuid::Uuid,
    activated_at: chrono::DateTime<chrono::Utc>,
}

#[derive(sqlx::FromRow)]
struct RouteRevisionOperationRow {
    route_revision_id: uuid::Uuid,
    operation: String,
}
