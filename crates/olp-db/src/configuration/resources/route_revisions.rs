use super::routes::RouteTargetRow;
use super::*;
use crate::audit_events::record_success;

impl Store {
    pub async fn list_route_revisions(
        &self,
        route_id: Uuid,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<ConfigurationPage<RouteRevisionRecord>, Error> {
        let limit = checked_limit(limit)?;
        let exists: bool = sqlx::query_scalar!(
            "SELECT EXISTS (SELECT 1 FROM routes WHERE id = $1) AS \"value!\"",
            route_id
        )
        .fetch_one(self.pool())
        .await?;
        if !exists {
            return Err(Error::NotFound);
        }
        let before_revision: Option<i32> = match cursor {
            Some(cursor) => Some(
                sqlx::query_scalar!(
                    "SELECT revision FROM route_revisions WHERE route_id = $1 AND id = $2",
                    route_id,
                    cursor
                )
                .fetch_optional(self.pool())
                .await?
                .ok_or_else(|| {
                    Error::Invalid("route-revision pagination cursor is invalid".to_owned())
                })?,
            ),
            None => None,
        };
        let ids: Vec<Uuid> = sqlx::query_scalar!(
            "SELECT id FROM route_revisions WHERE route_id = $1 \
             AND ($2::int IS NULL OR revision < $2) \
             ORDER BY revision DESC LIMIT $3",
            route_id,
            before_revision,
            limit + 1
        )
        .fetch_all(self.pool())
        .await?;
        let (ids, next_cursor) = split_page(ids, limit as usize, |id| *id);
        let mut revisions_map = self.get_route_revisions(&ids).await?;
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
        &self,
        revision_ids: &[Uuid],
    ) -> Result<BTreeMap<Uuid, RouteRevisionRecord>, Error> {
        if revision_ids.is_empty() {
            return Ok(BTreeMap::new());
        }

        let revision_rows = sqlx::query!(
            "SELECT id, routing_id, route_id, revision, slug, overall_timeout_ms, max_attempts, source_draft_id, \
                    activated_by, activated_at FROM route_revisions WHERE id = ANY($1::uuid[])",
            revision_ids
        )
        .fetch_all(self.pool())
        .await?;

        let operation_rows = sqlx::query!(
            "SELECT route_revision_id, operation FROM route_revision_operations \
             WHERE route_revision_id = ANY($1::uuid[]) ORDER BY route_revision_id, operation",
            revision_ids
        )
        .fetch_all(self.pool())
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

        let target_rows_raw = sqlx::query_as!(
            RouteRevisionTargetRow,
            // Revisions are immutable history. Read the target list from the
            // revision itself and join the provider's current revision only to
            // decorate availability, so a target whose model left that revision
            // is reported as unavailable instead of silently disappearing.
            "SELECT rrt.route_revision_id, rrt.id, rrt.routing_id, rrt.provider_model_id, \
                    p.id AS provider_id, COALESCE(pr.name, p.name) AS \"provider_name!\", \
                    COALESCE(prm.upstream_model, pm.upstream_model) AS \"provider_model!\", \
                    (p.state <> 'disabled'::provider_state AND prm.id IS NOT NULL \
                     AND COALESCE(prm.enabled, false)) AS \"available!\", \
                    rrt.priority, rrt.weight, rrt.timeout_ms, rrt.position \
             FROM route_revision_targets rrt \
             JOIN provider_models pm ON pm.id = rrt.provider_model_id \
             JOIN providers p ON p.id = pm.provider_id \
             LEFT JOIN provider_revisions pr ON pr.id = p.active_revision_id \
             LEFT JOIN provider_revision_models prm ON prm.provider_revision_id = pr.id \
               AND prm.source_provider_model_id = pm.id \
             WHERE rrt.route_revision_id = ANY($1::uuid[]) ORDER BY rrt.route_revision_id, rrt.position",
            revision_ids
        )
        .fetch_all(self.pool())
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
        &self,
        route_id: Uuid,
        revision_id: Uuid,
    ) -> Result<RouteRevisionRecord, Error> {
        let rev = self
            .get_route_revisions(&[revision_id])
            .await?
            .remove(&revision_id)
            .ok_or(Error::NotFound)?;
        if rev.route_id != route_id {
            return Err(Error::NotFound);
        }
        Ok(rev)
    }

    pub async fn diff_route_revisions(
        &self,
        route_id: Uuid,
        from_id: Uuid,
        to_id: Uuid,
    ) -> Result<RouteRevisionDiff, Error> {
        let from = self.get_route_revision(route_id, from_id).await?;
        let to = self.get_route_revision(route_id, to_id).await?;
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
        &self,
        route_id: Uuid,
        revision_id: Uuid,
        actor: Uuid,
        idempotency_key: &str,
    ) -> Result<RouteDraftRecord, Error> {
        let revision = self.get_route_revision(route_id, revision_id).await?;
        let mut transaction = self.pool().begin().await?;
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
        sqlx::query!(
            "INSERT INTO route_drafts \
             (id, routing_id, slug, state, overall_timeout_ms, max_attempts, etag, based_on_revision_id, created_by) \
             VALUES ($1, $2, $3, 'draft'::route_draft_state, $4, $5, $6, $7, $8)",
        id, revision.routing_id, &revision.slug, revision.overall_timeout_ms, revision.max_attempts, etag, revision_id, actor)
        .execute(&mut *transaction)
        .await?;
        sqlx::query!(
            "INSERT INTO route_draft_operations (route_draft_id, operation) \
             SELECT $1, operation FROM route_revision_operations WHERE route_revision_id = $2",
            id,
            revision_id
        )
        .execute(&mut *transaction)
        .await?;
        sqlx::query!(
            "INSERT INTO route_draft_targets \
             (id, routing_id, route_draft_id, provider_model_id, priority, weight, timeout_ms, position) \
             SELECT uuidv7(), routing_id, $1, provider_model_id, priority, weight, timeout_ms, position \
             FROM route_revision_targets WHERE route_revision_id = $2",
        id, revision_id)
        .execute(&mut *transaction)
        .await?;
        record_success(
            &mut *transaction,
            self.provenance(),
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
        self.get_route_draft(id).await
    }
}

/// `query_as!` fills fields positionally, so the revision key column that this
/// read prepends has to sit in the struct rather than in a nested one. The
/// remaining columns are exactly a [`RouteTargetRow`], which owns the mapping.
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
