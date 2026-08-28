use super::*;
use crate::audit_events::record_success;

impl Store {
    pub async fn list_route_drafts(
        &self,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<ConfigurationPage<RouteDraftRecord>, Error> {
        let limit = checked_limit(limit)?;
        let rows = sqlx::query!(
            "SELECT id FROM route_drafts WHERE ($1::uuid IS NULL OR id > $1) ORDER BY id LIMIT $2",
            cursor,
            limit + 1
        )
        .fetch_all(self.pool())
        .await?;
        let (rows, next_cursor) = split_page(rows, limit as usize, |row| row.id);
        let ids: Vec<Uuid> = rows.into_iter().map(|row| row.id).collect();
        let items = self.get_route_drafts(&ids).await?;
        Ok(ConfigurationPage { items, next_cursor })
    }

    /// Reads drafts in the order of `ids` with three queries for the whole
    /// set, mirroring [`Store::get_routes`]. Any missing id is `NotFound`.
    pub async fn get_route_drafts(&self, ids: &[Uuid]) -> Result<Vec<RouteDraftRecord>, Error> {
        if ids.is_empty() {
            return Ok(Vec::new());
        }
        let header_rows = sqlx::query!(
            "SELECT rd.id AS \"id!\", rd.routing_id AS \"routing_id!\", rd.slug AS \"slug!\", \
                    rd.state::text AS \"state!\", rd.overall_timeout_ms AS \"overall_timeout_ms!\", \
                    rd.max_attempts AS \"max_attempts!\", rd.etag AS \"etag!\", rd.based_on_revision_id, \
                    rd.created_at AS \"created_at!\", rd.updated_at AS \"updated_at!\", \
                    creator.email AS \"created_by_email?\" \
             FROM route_drafts rd LEFT JOIN users creator ON creator.id = rd.created_by \
             WHERE rd.id = ANY($1::uuid[])",
            ids
        )
        .fetch_all(self.pool())
        .await?;
        let mut operations_map = BTreeMap::<Uuid, Vec<OperationKind>>::new();
        for row in sqlx::query!(
            "SELECT route_draft_id, operation FROM route_draft_operations \
             WHERE route_draft_id = ANY($1::uuid[]) ORDER BY route_draft_id, operation",
            ids
        )
        .fetch_all(self.pool())
        .await?
        {
            let operation = row
                .operation
                .parse()
                .map_err(|_| PersistenceError::InvalidStoredValue("route draft operation"))?;
            operations_map
                .entry(row.route_draft_id)
                .or_default()
                .push(operation);
        }
        let mut targets_map = BTreeMap::<Uuid, Vec<RouteTargetRecord>>::new();
        for row in draft_targets(self.pool(), ids).await? {
            let (draft_id, target) = row.split();
            targets_map.entry(draft_id).or_default().push(target);
        }

        let mut headers = BTreeMap::new();
        for row in header_rows {
            headers.insert(row.id, row);
        }
        let mut items = Vec::with_capacity(ids.len());
        for id in ids {
            let row = headers.remove(id).ok_or(Error::NotFound)?;
            items.push(RouteDraftRecord {
                id: row.id,
                routing_id: row.routing_id,
                slug: row.slug,
                state: row
                    .state
                    .parse()
                    .map_err(|_| PersistenceError::InvalidStoredValue("route draft state"))?,
                overall_timeout_ms: row.overall_timeout_ms,
                max_attempts: row.max_attempts,
                etag: row.etag,
                based_on_revision_id: row.based_on_revision_id,
                operations: operations_map.remove(id).unwrap_or_default(),
                targets: targets_map.remove(id).unwrap_or_default(),
                created_at: row.created_at,
                updated_at: row.updated_at,
                created_by_email: row.created_by_email,
            });
        }
        Ok(items)
    }

    pub async fn get_route_draft(&self, draft_id: Uuid) -> Result<RouteDraftRecord, Error> {
        self.get_route_drafts(&[draft_id])
            .await?
            .into_iter()
            .next()
            .ok_or(Error::NotFound)
    }

    pub async fn replace_route_draft(
        &self,
        draft_id: Uuid,
        expected_etag: Uuid,
        input: &ReplaceRouteDraftInput,
        actor: Uuid,
    ) -> Result<Uuid, Error> {
        validate_route_input(
            &input.slug,
            &input.operations,
            input.overall_timeout_ms,
            input.max_attempts,
            &input.targets,
        )?;
        let mut transaction = self.pool().begin().await?;
        let lineage_slug: Option<String> = sqlx::query_scalar!(
            "SELECT rr.slug FROM route_drafts rd \
             JOIN route_revisions rr ON rr.id = rd.based_on_revision_id \
             WHERE rd.id = $1",
            draft_id
        )
        .fetch_optional(&mut *transaction)
        .await?;
        if lineage_slug
            .as_deref()
            .is_some_and(|lineage_slug| lineage_slug != input.slug.as_str())
        {
            return Err(Error::Invalid(
                "a restored route draft must retain its original stable slug".to_owned(),
            ));
        }
        let etag = Uuid::now_v7();
        let result = sqlx::query!(
            "UPDATE route_drafts SET slug = $1, overall_timeout_ms = $2, max_attempts = $3, \
                    state = 'draft'::route_draft_state, etag = $4, updated_at = now() \
             WHERE id = $5 AND etag = $6",
            &input.slug,
            input.overall_timeout_ms,
            input.max_attempts,
            etag,
            draft_id,
            expected_etag
        )
        .execute(&mut *transaction)
        .await?;
        if result.rows_affected() != 1 {
            let exists: bool = sqlx::query_scalar!(
                "SELECT EXISTS (SELECT 1 FROM route_drafts WHERE id = $1) AS \"value!\"",
                draft_id
            )
            .fetch_one(&mut *transaction)
            .await?;
            return Err(if exists {
                Error::PreconditionFailed
            } else {
                Error::NotFound
            });
        }
        let previous_targets = sqlx::query!(
            "SELECT routing_id, provider_model_id, priority, weight, timeout_ms, position \
             FROM route_draft_targets WHERE route_draft_id = $1 ORDER BY position",
            draft_id
        )
        .fetch_all(&mut *transaction)
        .await?;
        sqlx::query!(
            "DELETE FROM route_draft_operations WHERE route_draft_id = $1",
            draft_id
        )
        .execute(&mut *transaction)
        .await?;
        sqlx::query!(
            "DELETE FROM route_draft_targets WHERE route_draft_id = $1",
            draft_id
        )
        .execute(&mut *transaction)
        .await?;
        for operation in &input.operations {
            sqlx::query!(
                "INSERT INTO route_draft_operations (route_draft_id, operation) VALUES ($1, $2)",
                draft_id,
                operation.as_str()
            )
            .execute(&mut *transaction)
            .await?;
        }
        for (position, (provider_model_id, priority, weight, timeout_ms)) in
            input.targets.iter().enumerate()
        {
            let position = i32::try_from(position)
                .map_err(|_| Error::Invalid("too many targets".to_owned()))?;
            let enabled: bool = sqlx::query_scalar!(
                "SELECT EXISTS (SELECT 1 FROM providers p \
                 JOIN provider_revision_models prm ON prm.provider_revision_id = p.active_revision_id \
                 WHERE prm.source_provider_model_id = $1 AND prm.enabled \
                   AND p.state <> 'disabled'::provider_state) AS \"value!\"",
            provider_model_id)
            .fetch_one(&mut *transaction)
            .await?;
            if !enabled {
                return Err(Error::Invalid(format!(
                    "provider model {provider_model_id} is not active"
                )));
            }
            let routing_id = previous_targets
                .iter()
                .find(|target| {
                    target.position == position
                        && target.provider_model_id == *provider_model_id
                        && target.priority == *priority
                        && target.weight == *weight
                        && target.timeout_ms == *timeout_ms
                })
                .map_or_else(Uuid::now_v7, |target| target.routing_id);
            sqlx::query!(
                "INSERT INTO route_draft_targets \
                  (id, routing_id, route_draft_id, provider_model_id, priority, weight, timeout_ms, position) \
                  VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
            Uuid::now_v7(), routing_id, draft_id, provider_model_id, priority, weight, timeout_ms, position)
            .execute(&mut *transaction)
            .await?;
        }
        record_success(
            &mut *transaction,
            self.provenance(),
            actor,
            "route.update_draft",
            "route_draft",
            draft_id,
        )
        .await?;
        transaction.commit().await?;
        Ok(etag)
    }

    pub async fn delete_route_draft(
        &self,
        draft_id: Uuid,
        expected_etag: Uuid,
        actor: Uuid,
    ) -> Result<(), Error> {
        let mut transaction = self.pool().begin().await?;
        let referenced: bool = sqlx::query_scalar!(
            "SELECT EXISTS (SELECT 1 FROM route_revisions WHERE source_draft_id = $1) AS \"value!\"",
            draft_id
        )
        .fetch_one(&mut *transaction)
        .await?;
        if referenced {
            return Err(Error::InUse);
        }
        let result = sqlx::query!(
            "DELETE FROM route_drafts WHERE id = $1 AND etag = $2",
            draft_id,
            expected_etag
        )
        .execute(&mut *transaction)
        .await?;
        if result.rows_affected() != 1 {
            let exists: bool = sqlx::query_scalar!(
                "SELECT EXISTS (SELECT 1 FROM route_drafts WHERE id = $1) AS \"value!\"",
                draft_id
            )
            .fetch_one(&mut *transaction)
            .await?;
            return Err(if exists {
                Error::PreconditionFailed
            } else {
                Error::NotFound
            });
        }
        record_success(
            &mut *transaction,
            self.provenance(),
            actor,
            "route.delete_draft",
            "route_draft",
            draft_id,
        )
        .await?;
        transaction.commit().await?;
        Ok(())
    }

    pub async fn simulate_route_draft(
        &self,
        draft_id: Uuid,
        operation: OperationKind,
        surface: Surface,
        mode: TransportMode,
        seed: &str,
    ) -> Result<RouteSimulation, Error> {
        if seed.is_empty() || seed.len() > 256 {
            return Err(Error::Invalid(
                "simulation seed must contain 1-256 bytes".to_owned(),
            ));
        }
        let draft = self.get_route_draft(draft_id).await?;
        if !draft.operations.contains(&operation) {
            return Err(Error::Invalid(format!(
                "route does not support {operation}"
            )));
        }
        let scoring_route_id = RouteId::from_uuid(draft.routing_id);
        let maximum = usize::try_from(draft.max_attempts).unwrap_or_default();
        let mut ranked: BTreeMap<i32, Vec<(f64, RouteTargetRecord)>> = BTreeMap::new();
        let mut ineligible = Vec::new();
        for target in draft.targets {
            let capability: bool = sqlx::query_scalar!(
                "SELECT EXISTS (SELECT 1 FROM providers p \
                 JOIN provider_revision_models prm ON prm.provider_revision_id = p.active_revision_id \
                 JOIN provider_revision_capabilities prc \
                   ON prc.provider_revision_model_id = prm.id \
                 WHERE prm.source_provider_model_id = $1 AND prc.operation = $2 \
                   AND prc.surface = $3 AND prc.mode = $4 AND prm.enabled \
                   AND prc.source = 'certified' AND p.state <> 'disabled'::provider_state) AS \"value!\"",
            target.provider_model_id, operation.as_str(), surface.as_str(), mode.as_str())
            .fetch_one(self.pool())
            .await?;
            if capability {
                let weight = u32::try_from(target.weight)
                    .ok()
                    .and_then(NonZeroU32::new)
                    .ok_or_else(|| Error::Invalid("route target weight is invalid".to_owned()))?;
                let score = weighted_rendezvous_score(
                    scoring_route_id,
                    TargetId::from_uuid(target.routing_id),
                    weight,
                    operation,
                    surface,
                    mode,
                    seed.as_bytes(),
                );
                ranked
                    .entry(target.priority)
                    .or_default()
                    .push((score, target));
            } else {
                ineligible.push(RouteSimulationTarget {
                    target_id: target.id,
                    provider_id: target.provider_id,
                    provider_name: target.provider_name,
                    upstream_model: target.upstream_model,
                    priority: target.priority,
                    eligible: false,
                    reason: Some(
                        "missing exact capability or provider/model is disabled".to_owned(),
                    ),
                    attempt: None,
                });
            }
        }
        let mut targets = Vec::new();
        for (_, mut group) in ranked {
            group.sort_by(|left, right| {
                right
                    .0
                    .total_cmp(&left.0)
                    .then_with(|| left.1.routing_id.cmp(&right.1.routing_id))
            });
            for (_, target) in group {
                let attempt = (targets.len() < maximum).then_some(targets.len() + 1);
                targets.push(RouteSimulationTarget {
                    target_id: target.id,
                    provider_id: target.provider_id,
                    provider_name: target.provider_name,
                    upstream_model: target.upstream_model,
                    priority: target.priority,
                    eligible: true,
                    reason: attempt
                        .is_none()
                        .then(|| "eligible but beyond max_attempts".to_owned()),
                    attempt,
                });
            }
        }
        targets.extend(ineligible);
        Ok(RouteSimulation {
            deterministic_seed: seed.to_owned(),
            operation,
            surface,
            mode,
            targets,
        })
    }

    pub async fn list_routes(
        &self,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<ConfigurationPage<RouteRecord>, Error> {
        let limit = checked_limit(limit)?;
        let rows = sqlx::query!(
            "SELECT id FROM routes WHERE ($1::uuid IS NULL OR id > $1)
             ORDER BY id LIMIT $2",
            cursor,
            limit + 1
        )
        .fetch_all(self.pool())
        .await?;
        let (rows, next_cursor) = split_page(rows, limit as usize, |row| row.id);
        let ids = rows.into_iter().map(|row| row.id).collect::<Vec<_>>();
        let items = self.get_routes(&ids).await?;
        Ok(ConfigurationPage { items, next_cursor })
    }

    pub async fn get_routes(&self, ids: &[Uuid]) -> Result<Vec<RouteRecord>, Error> {
        if ids.is_empty() {
            return Ok(Vec::new());
        }

        let route_rows = sqlx::query!(
            "SELECT r.id AS \"id!\", r.slug AS \"slug!\", r.created_at AS \"created_at!\",
                    creator.email AS \"created_by_email?\",
                    (SELECT rr.id FROM route_revisions rr WHERE rr.route_id = r.id
                     ORDER BY rr.revision DESC LIMIT 1) AS latest_revision_id,
                    (SELECT count(*) FROM route_revisions rr WHERE rr.route_id = r.id)::bigint
                      AS \"revision_count!\"
             FROM routes r LEFT JOIN users creator ON creator.id = r.created_by
             WHERE r.id = ANY($1::uuid[])",
            ids
        )
        .fetch_all(self.pool())
        .await?;

        let mut route_map = BTreeMap::new();
        let mut revision_ids = Vec::new();
        for row in route_rows {
            let latest_revision_id = row.latest_revision_id.ok_or_else(|| {
                Error::Invalid("activated route has no immutable revision".to_owned())
            })?;
            let revision_count = u64::try_from(row.revision_count)
                .map_err(|_| Error::Invalid("route revision count is invalid".to_owned()))?;
            revision_ids.push(latest_revision_id);
            route_map.insert(
                row.id,
                RouteHeader {
                    slug: row.slug,
                    created_at: row.created_at,
                    created_by_email: row.created_by_email,
                    revision_count,
                    latest_revision_id,
                },
            );
        }

        let mut revisions_map = self.get_route_revisions(&revision_ids).await?;

        let mut items = Vec::with_capacity(ids.len());
        for id in ids {
            let header = route_map.remove(id).ok_or(Error::NotFound)?;
            let latest_revision = revisions_map
                .remove(&header.latest_revision_id)
                .ok_or(Error::NotFound)?;
            items.push(RouteRecord {
                id: *id,
                slug: header.slug,
                created_at: header.created_at,
                created_by_email: header.created_by_email,
                revision_count: header.revision_count,
                latest_revision,
            });
        }
        Ok(items)
    }

    pub async fn get_route(&self, id: Uuid) -> Result<RouteRecord, Error> {
        self.get_routes(&[id])
            .await?
            .into_iter()
            .next()
            .ok_or(Error::NotFound)
    }
}

async fn draft_targets(
    pool: &sqlx::PgPool,
    ids: &[Uuid],
) -> Result<Vec<RouteDraftTargetRow>, Error> {
    Ok(sqlx::query_as!(
        RouteDraftTargetRow,
        // A draft read must return every stored target: the console writes
        // back what it reads, so a target hidden by an inner join would be
        // deleted by the next replace. Availability is decoration only.
        "SELECT rdt.route_draft_id, rdt.id, rdt.routing_id, rdt.provider_model_id, \
                    p.id AS provider_id, COALESCE(pr.name, p.name) AS \"provider_name!\", \
                    COALESCE(prm.upstream_model, pm.upstream_model) AS \"provider_model!\", \
                    (p.state <> 'disabled'::provider_state AND prm.id IS NOT NULL \
                     AND COALESCE(prm.enabled, false)) AS \"available!\", \
                    rdt.priority, rdt.weight, rdt.timeout_ms, rdt.position \
             FROM route_draft_targets rdt \
             JOIN provider_models pm ON pm.id = rdt.provider_model_id \
             JOIN providers p ON p.id = pm.provider_id \
             LEFT JOIN provider_revisions pr ON pr.id = p.active_revision_id \
             LEFT JOIN provider_revision_models prm ON prm.provider_revision_id = pr.id \
               AND prm.source_provider_model_id = pm.id \
             WHERE rdt.route_draft_id = ANY($1::uuid[]) ORDER BY rdt.route_draft_id, rdt.position",
        ids
    )
    .fetch_all(pool)
    .await?)
}

/// `query_as!` fills fields positionally, so the draft key column sits in
/// the struct; the remaining columns are exactly a [`RouteTargetRow`].
#[derive(Debug, sqlx::FromRow)]
struct RouteDraftTargetRow {
    route_draft_id: Uuid,
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

impl RouteDraftTargetRow {
    fn split(self) -> (Uuid, RouteTargetRecord) {
        let Self {
            route_draft_id,
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
        (route_draft_id, target.into())
    }
}

struct RouteHeader {
    slug: String,
    created_at: DateTime<Utc>,
    created_by_email: Option<String>,
    revision_count: u64,
    latest_revision_id: Uuid,
}

/// The target columns every route read selects, draft or revision, under the
/// names the SQL gives them. Revision reads prepend their own key column and
/// hand the rest here, so the row-to-record mapping lives in one place.
#[derive(Debug, sqlx::FromRow)]
pub(super) struct RouteTargetRow {
    pub(super) id: Uuid,
    pub(super) routing_id: Uuid,
    pub(super) provider_model_id: Uuid,
    pub(super) provider_id: Uuid,
    pub(super) provider_name: String,
    pub(super) provider_model: String,
    pub(super) available: bool,
    pub(super) priority: i32,
    pub(super) weight: i32,
    pub(super) timeout_ms: i32,
    pub(super) position: i32,
}

impl From<RouteTargetRow> for RouteTargetRecord {
    fn from(row: RouteTargetRow) -> Self {
        Self {
            id: row.id,
            routing_id: row.routing_id,
            provider_model_id: row.provider_model_id,
            provider_id: row.provider_id,
            provider_name: row.provider_name,
            upstream_model: row.provider_model,
            available: row.available,
            priority: row.priority,
            weight: row.weight,
            timeout_ms: row.timeout_ms,
            position: row.position,
        }
    }
}
