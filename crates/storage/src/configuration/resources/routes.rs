use std::collections::HashMap;

use super::{helpers::audit_in_transaction, *};

impl PgStore {
    pub async fn list_route_drafts(
        &self,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<ConfigurationPage<RouteDraftRecord>, ConfigurationError> {
        let limit = checked_limit(limit)?;
        let rows = sqlx::query!(
            "SELECT id, routing_id, slug, state::text AS \"state!\", overall_timeout_ms, \
                    max_attempts, etag, based_on_revision_id, created_at, updated_at \
             FROM route_drafts WHERE ($1::uuid IS NULL OR id > $1) ORDER BY id LIMIT $2",
            cursor,
            limit + 1
        )
        .fetch_all(self.pool())
        .await?;
        let (rows, next_cursor) = split_page(rows, limit as usize, |row| row.id);
        let ids: Vec<_> = rows.iter().map(|row| row.id).collect();
        let operation_rows = sqlx::query!(
            "SELECT route_draft_id, operation::text AS \"operation!\" \
             FROM route_draft_operations WHERE route_draft_id = ANY($1) \
             ORDER BY route_draft_id, operation",
            &ids
        )
        .fetch_all(self.pool())
        .await?;
        let mut operations = HashMap::<Uuid, Vec<OperationKind>>::new();
        for row in operation_rows {
            operations.entry(row.route_draft_id).or_default().push(
                row.operation
                    .parse()
                    .map_err(|_| PersistenceError::InvalidStoredValue("route draft operation"))?,
            );
        }
        let target_rows = sqlx::query!(
            "SELECT rdt.route_draft_id, rdt.id, rdt.routing_id, rdt.provider_model_id, \
                    p.id AS provider_id, pr.name AS provider_name, \
                    prm.upstream_model AS provider_model, rdt.priority, rdt.weight, \
                    rdt.timeout_ms, rdt.position \
             FROM route_draft_targets rdt \
             JOIN provider_models pm ON pm.id = rdt.provider_model_id \
             JOIN providers p ON p.id = pm.provider_id \
             JOIN provider_revisions pr ON pr.id = p.active_revision_id \
             JOIN provider_revision_models prm ON prm.provider_revision_id = pr.id \
               AND prm.source_provider_model_id = pm.id \
             WHERE rdt.route_draft_id = ANY($1) ORDER BY rdt.route_draft_id, rdt.position",
            &ids
        )
        .fetch_all(self.pool())
        .await?;
        let mut targets = HashMap::<Uuid, Vec<RouteTargetRecord>>::new();
        for row in target_rows {
            targets
                .entry(row.route_draft_id)
                .or_default()
                .push(RouteTargetRecord {
                    id: row.id,
                    routing_id: row.routing_id,
                    provider_model_id: row.provider_model_id,
                    provider_id: row.provider_id,
                    provider_name: row.provider_name,
                    upstream_model: row.provider_model,
                    priority: row.priority,
                    weight: row.weight,
                    timeout_ms: row.timeout_ms,
                    position: row.position,
                });
        }
        let items =
            rows.into_iter()
                .map(|row| {
                    Ok(RouteDraftRecord {
                        id: row.id,
                        routing_id: row.routing_id,
                        slug: row.slug,
                        state: row.state.parse().map_err(|_| {
                            PersistenceError::InvalidStoredValue("route draft state")
                        })?,
                        overall_timeout_ms: row.overall_timeout_ms,
                        max_attempts: row.max_attempts,
                        etag: row.etag,
                        based_on_revision_id: row.based_on_revision_id,
                        operations: operations.remove(&row.id).unwrap_or_default(),
                        targets: targets.remove(&row.id).unwrap_or_default(),
                        created_at: row.created_at,
                        updated_at: row.updated_at,
                    })
                })
                .collect::<Result<_, ConfigurationError>>()?;
        Ok(ConfigurationPage { items, next_cursor })
    }

    pub async fn get_route_draft(
        &self,
        draft_id: Uuid,
    ) -> Result<RouteDraftRecord, ConfigurationError> {
        let row = sqlx::query!(
            "SELECT id, routing_id, slug, state::text AS \"state!\", overall_timeout_ms, max_attempts, etag, \
                    based_on_revision_id, created_at, updated_at FROM route_drafts WHERE id = $1",
        draft_id)
        .fetch_optional(self.pool())
        .await?
        .ok_or(ConfigurationError::NotFound)?;
        Ok(RouteDraftRecord {
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
            operations: draft_operations(self.pool(), draft_id).await?,
            targets: draft_targets(self.pool(), draft_id).await?,
            created_at: row.created_at,
            updated_at: row.updated_at,
        })
    }

    pub async fn replace_route_draft(
        &self,
        draft_id: Uuid,
        expected_etag: Uuid,
        input: &ReplaceRouteDraftInput,
        actor: Uuid,
    ) -> Result<Uuid, ConfigurationError> {
        validate_route_input(
            &input.slug,
            &input.operations,
            input.overall_timeout_ms,
            input.max_attempts,
            &input.targets,
        )?;
        let mut transaction = self.pool().begin().await?;
        sqlx::query_scalar!("SELECT set_config('transaction_timeout', '30s', true)")
            .fetch_one(&mut *transaction)
            .await?;
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
            return Err(ConfigurationError::Invalid(
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
                ConfigurationError::PreconditionFailed
            } else {
                ConfigurationError::NotFound
            });
        }
        let operation_names: Vec<_> = input
            .operations
            .iter()
            .map(|operation| operation.as_str().to_owned())
            .collect();
        let provider_model_ids: Vec<_> = input
            .targets
            .iter()
            .map(|(provider_model_id, _, _, _)| *provider_model_id)
            .collect();
        let invalid_provider_model_id = sqlx::query_scalar!(
            "SELECT input.provider_model_id AS \"provider_model_id!\" \
             FROM unnest($1::uuid[]) AS input(provider_model_id) \
             WHERE NOT EXISTS ( \
                 SELECT 1 FROM providers p \
                 JOIN provider_revision_models prm \
                   ON prm.provider_revision_id = p.active_revision_id \
                 WHERE prm.source_provider_model_id = input.provider_model_id AND prm.enabled \
                   AND p.state <> 'disabled'::provider_state \
             ) LIMIT 1",
            &provider_model_ids
        )
        .fetch_optional(&mut *transaction)
        .await?;
        if let Some(provider_model_id) = invalid_provider_model_id {
            return Err(ConfigurationError::Invalid(format!(
                "provider model {provider_model_id} is not active"
            )));
        }
        sqlx::query!(
            "DELETE FROM route_draft_operations \
             WHERE route_draft_id = $1 AND NOT operation = ANY($2::text[])",
            draft_id,
            &operation_names
        )
        .execute(&mut *transaction)
        .await?;
        sqlx::query!(
            "INSERT INTO route_draft_operations (route_draft_id, operation) \
             SELECT $1, input.operation FROM unnest($2::text[]) AS input(operation) \
             ON CONFLICT (route_draft_id, operation) DO NOTHING",
            draft_id,
            &operation_names
        )
        .execute(&mut *transaction)
        .await?;
        let target_ids: Vec<_> = input.targets.iter().map(|_| Uuid::now_v7()).collect();
        let target_routing_ids: Vec<_> = input.targets.iter().map(|_| Uuid::now_v7()).collect();
        let priorities: Vec<_> = input
            .targets
            .iter()
            .map(|(_, priority, _, _)| *priority)
            .collect();
        let weights: Vec<_> = input
            .targets
            .iter()
            .map(|(_, _, weight, _)| *weight)
            .collect();
        let timeouts: Vec<_> = input
            .targets
            .iter()
            .map(|(_, _, _, timeout)| *timeout)
            .collect();
        let positions: Vec<_> = (0..input.targets.len())
            .map(|position| {
                i32::try_from(position)
                    .expect("route target cardinality is validated before database access")
            })
            .collect();
        sqlx::query!(
            "DELETE FROM route_draft_targets \
             WHERE route_draft_id = $1 AND position >= $2",
            draft_id,
            i32::try_from(input.targets.len())
                .expect("route target cardinality is validated before database access")
        )
        .execute(&mut *transaction)
        .await?;
        sqlx::query!(
            "INSERT INTO route_draft_targets \
             (id, routing_id, route_draft_id, provider_model_id, priority, weight, timeout_ms, position) \
             SELECT input.id, input.routing_id, $1, input.provider_model_id, input.priority, \
                    input.weight, input.timeout_ms, input.position \
             FROM unnest($2::uuid[], $3::uuid[], $4::uuid[], $5::int[], \
                         $6::int[], $7::int[], $8::int[]) \
                  AS input(id, routing_id, provider_model_id, priority, weight, timeout_ms, position) \
             ON CONFLICT (route_draft_id, position) DO UPDATE SET \
                 id = EXCLUDED.id, \
                 routing_id = CASE WHEN \
                     (route_draft_targets.provider_model_id, route_draft_targets.priority, \
                      route_draft_targets.weight, route_draft_targets.timeout_ms) \
                     IS NOT DISTINCT FROM \
                     (EXCLUDED.provider_model_id, EXCLUDED.priority, \
                      EXCLUDED.weight, EXCLUDED.timeout_ms) \
                   THEN route_draft_targets.routing_id ELSE EXCLUDED.routing_id END, \
                 provider_model_id = EXCLUDED.provider_model_id, priority = EXCLUDED.priority, \
                 weight = EXCLUDED.weight, timeout_ms = EXCLUDED.timeout_ms",
            draft_id,
            &target_ids,
            &target_routing_ids,
            &provider_model_ids,
            &priorities,
            &weights,
            &timeouts,
            &positions
        )
        .execute(&mut *transaction)
        .await?;
        audit_in_transaction(
            &mut transaction,
            actor,
            "route.update_draft",
            "route_draft",
            draft_id,
            "success",
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
    ) -> Result<(), ConfigurationError> {
        let mut transaction = self.pool().begin().await?;
        let referenced: bool = sqlx::query_scalar!(
            "SELECT EXISTS (SELECT 1 FROM route_revisions WHERE source_draft_id = $1) AS \"value!\"",
            draft_id
        )
        .fetch_one(&mut *transaction)
        .await?;
        if referenced {
            return Err(ConfigurationError::InUse);
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
                ConfigurationError::PreconditionFailed
            } else {
                ConfigurationError::NotFound
            });
        }
        audit_in_transaction(
            &mut transaction,
            actor,
            "route.delete_draft",
            "route_draft",
            draft_id,
            "success",
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
    ) -> Result<RouteSimulation, ConfigurationError> {
        if seed.is_empty() || seed.len() > 256 {
            return Err(ConfigurationError::Invalid(
                "simulation seed must contain 1-256 bytes".to_owned(),
            ));
        }
        let draft = self.get_route_draft(draft_id).await?;
        if !draft.operations.contains(&operation) {
            return Err(ConfigurationError::Invalid(format!(
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
                    .ok_or_else(|| {
                        ConfigurationError::Invalid("route target weight is invalid".to_owned())
                    })?;
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

    pub async fn list_route_revisions(
        &self,
        route_id: Uuid,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<ConfigurationPage<RouteRevisionRecord>, ConfigurationError> {
        let limit = checked_limit(limit)?;
        let exists: bool = sqlx::query_scalar!(
            "SELECT EXISTS (SELECT 1 FROM routes WHERE id = $1) AS \"value!\"",
            route_id
        )
        .fetch_one(self.pool())
        .await?;
        if !exists {
            return Err(ConfigurationError::NotFound);
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
                    ConfigurationError::Invalid(
                        "route-revision pagination cursor is invalid".to_owned(),
                    )
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
        let mut records = revision_records_by_ids(self.pool(), &ids).await?;
        let revisions = ids
            .into_iter()
            .map(|id| records.remove(&id).ok_or(ConfigurationError::NotFound))
            .collect::<Result<_, _>>()?;
        Ok(ConfigurationPage {
            items: revisions,
            next_cursor,
        })
    }

    pub async fn list_routes(
        &self,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<ConfigurationPage<RouteRecord>, ConfigurationError> {
        let limit = checked_limit(limit)?;
        let rows = sqlx::query!(
            "SELECT r.id, r.slug, r.created_at, \
                    (SELECT rr.id FROM route_revisions rr WHERE rr.route_id = r.id \
                     ORDER BY rr.revision DESC LIMIT 1) AS latest_revision_id, \
                    (SELECT count(*) FROM route_revisions rr WHERE rr.route_id = r.id)::bigint \
                      AS \"revision_count!\" \
             FROM routes r WHERE ($1::uuid IS NULL OR r.id > $1) \
             ORDER BY r.id LIMIT $2",
            cursor,
            limit + 1
        )
        .fetch_all(self.pool())
        .await?;
        let (rows, next_cursor) = split_page(rows, limit as usize, |row| row.id);
        let revision_ids = rows
            .iter()
            .map(|row| {
                row.latest_revision_id.ok_or_else(|| {
                    ConfigurationError::Invalid(
                        "activated route has no immutable revision".to_owned(),
                    )
                })
            })
            .collect::<Result<Vec<_>, _>>()?;
        let mut revisions = revision_records_by_ids(self.pool(), &revision_ids).await?;
        let items = rows
            .into_iter()
            .zip(revision_ids)
            .map(|(row, revision_id)| {
                Ok(RouteRecord {
                    id: row.id,
                    slug: row.slug,
                    created_at: row.created_at,
                    revision_count: u64::try_from(row.revision_count).map_err(|_| {
                        ConfigurationError::Invalid("route revision count is invalid".to_owned())
                    })?,
                    latest_revision: revisions
                        .remove(&revision_id)
                        .ok_or(ConfigurationError::NotFound)?,
                })
            })
            .collect::<Result<_, ConfigurationError>>()?;
        Ok(ConfigurationPage { items, next_cursor })
    }

    pub async fn get_route(&self, id: Uuid) -> Result<RouteRecord, ConfigurationError> {
        let row = sqlx::query!(
            "SELECT r.id, r.slug, r.created_at,
                    (SELECT rr.id FROM route_revisions rr WHERE rr.route_id = r.id
                     ORDER BY rr.revision DESC LIMIT 1) AS latest_revision_id,
                    (SELECT count(*) FROM route_revisions rr WHERE rr.route_id = r.id)::bigint
                      AS \"revision_count!\"
             FROM routes r WHERE r.id = $1",
            id
        )
        .fetch_optional(self.pool())
        .await?
        .ok_or(ConfigurationError::NotFound)?;
        let latest_revision_id: Option<Uuid> = row.latest_revision_id;
        let latest_revision_id = latest_revision_id.ok_or_else(|| {
            ConfigurationError::Invalid("activated route has no immutable revision".to_owned())
        })?;
        let revision_count = u64::try_from(row.revision_count).map_err(|_| {
            ConfigurationError::Invalid("route revision count is invalid".to_owned())
        })?;
        Ok(RouteRecord {
            id: row.id,
            slug: row.slug,
            created_at: row.created_at,
            revision_count,
            latest_revision: self.get_route_revision(id, latest_revision_id).await?,
        })
    }

    pub async fn get_route_revision(
        &self,
        route_id: Uuid,
        revision_id: Uuid,
    ) -> Result<RouteRevisionRecord, ConfigurationError> {
        let row = sqlx::query!(
            "SELECT id, routing_id, route_id, revision, slug, overall_timeout_ms, max_attempts, source_draft_id, \
                    activated_by, activated_at FROM route_revisions WHERE route_id = $1 AND id = $2",
        route_id, revision_id)
        .fetch_optional(self.pool())
        .await?
        .ok_or(ConfigurationError::NotFound)?;
        Ok(RouteRevisionRecord {
            id: row.id,
            routing_id: row.routing_id,
            route_id: row.route_id,
            revision: row.revision,
            slug: row.slug,
            overall_timeout_ms: row.overall_timeout_ms,
            max_attempts: row.max_attempts,
            source_draft_id: row.source_draft_id,
            activated_by: row.activated_by,
            activated_at: row.activated_at,
            operations: revision_operations(self.pool(), revision_id).await?,
            targets: revision_targets(self.pool(), revision_id).await?,
        })
    }

    pub async fn diff_route_revisions(
        &self,
        route_id: Uuid,
        from_id: Uuid,
        to_id: Uuid,
    ) -> Result<RouteRevisionDiff, ConfigurationError> {
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
    ) -> Result<RouteDraftRecord, ConfigurationError> {
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
            return Err(ConfigurationError::IdempotencyConflict);
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
        audit_in_transaction(
            &mut transaction,
            actor,
            "route.restore_as_draft",
            "route_draft",
            id,
            "success",
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

async fn revision_records_by_ids(
    pool: &sqlx::PgPool,
    ids: &[Uuid],
) -> Result<HashMap<Uuid, RouteRevisionRecord>, ConfigurationError> {
    let rows = sqlx::query_as!(
        RouteRevisionRow,
        "SELECT id, routing_id, route_id, revision, slug, overall_timeout_ms, max_attempts, \
                source_draft_id, activated_by, activated_at \
         FROM route_revisions WHERE id = ANY($1)",
        ids
    )
    .fetch_all(pool)
    .await?;
    let operation_rows = sqlx::query!(
        "SELECT route_revision_id, operation::text AS \"operation!\" \
         FROM route_revision_operations WHERE route_revision_id = ANY($1) \
         ORDER BY route_revision_id, operation",
        ids
    )
    .fetch_all(pool)
    .await?;
    let mut operations = HashMap::<Uuid, Vec<OperationKind>>::new();
    for row in operation_rows {
        operations.entry(row.route_revision_id).or_default().push(
            row.operation
                .parse()
                .map_err(|_| PersistenceError::InvalidStoredValue("route operation"))?,
        );
    }
    let target_rows = sqlx::query!(
        "SELECT rrt.route_revision_id, rrt.id, rrt.routing_id, rrt.provider_model_id, \
                p.id AS provider_id, pr.name AS provider_name, \
                prm.upstream_model AS provider_model, rrt.priority, rrt.weight, \
                rrt.timeout_ms, rrt.position \
         FROM route_revision_targets rrt \
         JOIN provider_models pm ON pm.id = rrt.provider_model_id \
         JOIN providers p ON p.id = pm.provider_id \
         JOIN provider_revisions pr ON pr.id = p.active_revision_id \
         JOIN provider_revision_models prm ON prm.provider_revision_id = pr.id \
           AND prm.source_provider_model_id = pm.id \
         WHERE rrt.route_revision_id = ANY($1) \
         ORDER BY rrt.route_revision_id, rrt.position",
        ids
    )
    .fetch_all(pool)
    .await?;
    let mut targets = HashMap::<Uuid, Vec<RouteTargetRecord>>::new();
    for row in target_rows {
        targets
            .entry(row.route_revision_id)
            .or_default()
            .push(RouteTargetRecord {
                id: row.id,
                routing_id: row.routing_id,
                provider_model_id: row.provider_model_id,
                provider_id: row.provider_id,
                provider_name: row.provider_name,
                upstream_model: row.provider_model,
                priority: row.priority,
                weight: row.weight,
                timeout_ms: row.timeout_ms,
                position: row.position,
            });
    }
    rows.into_iter()
        .map(|row| {
            let id = row.id;
            Ok((
                id,
                RouteRevisionRecord {
                    id,
                    routing_id: row.routing_id,
                    route_id: row.route_id,
                    revision: row.revision,
                    slug: row.slug,
                    overall_timeout_ms: row.overall_timeout_ms,
                    max_attempts: row.max_attempts,
                    source_draft_id: row.source_draft_id,
                    activated_by: row.activated_by,
                    activated_at: row.activated_at,
                    operations: operations.remove(&id).unwrap_or_default(),
                    targets: targets.remove(&id).unwrap_or_default(),
                },
            ))
        })
        .collect()
}

async fn draft_operations(
    pool: &sqlx::PgPool,
    id: Uuid,
) -> Result<Vec<OperationKind>, ConfigurationError> {
    sqlx::query_scalar!(
        "SELECT operation FROM route_draft_operations WHERE route_draft_id = $1 ORDER BY operation",
        id
    )
    .fetch_all(pool)
    .await?
    .into_iter()
    .map(|value: String| {
        value
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("route draft operation").into())
    })
    .collect()
}

async fn revision_operations(
    pool: &sqlx::PgPool,
    id: Uuid,
) -> Result<Vec<OperationKind>, ConfigurationError> {
    sqlx::query_scalar!("SELECT operation FROM route_revision_operations WHERE route_revision_id = $1 ORDER BY operation", id).fetch_all(pool).await?
        .into_iter()
        .map(|value: String| value.parse().map_err(|_| PersistenceError::InvalidStoredValue("route revision operation").into()))
        .collect()
}

async fn draft_targets(
    pool: &sqlx::PgPool,
    id: Uuid,
) -> Result<Vec<RouteTargetRecord>, ConfigurationError> {
    target_rows(
        sqlx::query_as!(
            RouteTargetRow,
            "SELECT rdt.id, rdt.routing_id, rdt.provider_model_id, p.id AS provider_id, pr.name AS provider_name, \
                    prm.upstream_model AS provider_model, rdt.priority, rdt.weight, rdt.timeout_ms, rdt.position \
             FROM route_draft_targets rdt \
             JOIN provider_models pm ON pm.id = rdt.provider_model_id \
             JOIN providers p ON p.id = pm.provider_id \
             JOIN provider_revisions pr ON pr.id = p.active_revision_id \
             JOIN provider_revision_models prm ON prm.provider_revision_id = pr.id \
               AND prm.source_provider_model_id = pm.id \
             WHERE rdt.route_draft_id = $1 ORDER BY rdt.position",
        id).fetch_all(pool).await?
    )
}

async fn revision_targets(
    pool: &sqlx::PgPool,
    id: Uuid,
) -> Result<Vec<RouteTargetRecord>, ConfigurationError> {
    target_rows(
        sqlx::query_as!(
            RouteTargetRow,
            "SELECT rrt.id, rrt.routing_id, rrt.provider_model_id, p.id AS provider_id, pr.name AS provider_name, \
                    prm.upstream_model AS provider_model, rrt.priority, rrt.weight, rrt.timeout_ms, rrt.position \
             FROM route_revision_targets rrt \
             JOIN provider_models pm ON pm.id = rrt.provider_model_id \
             JOIN providers p ON p.id = pm.provider_id \
             JOIN provider_revisions pr ON pr.id = p.active_revision_id \
             JOIN provider_revision_models prm ON prm.provider_revision_id = pr.id \
               AND prm.source_provider_model_id = pm.id \
             WHERE rrt.route_revision_id = $1 ORDER BY rrt.position",
        id).fetch_all(pool).await?
    )
}

#[derive(Debug, sqlx::FromRow)]
struct RouteRevisionRow {
    id: Uuid,
    routing_id: Uuid,
    route_id: Uuid,
    revision: i32,
    slug: String,
    overall_timeout_ms: i32,
    max_attempts: i16,
    source_draft_id: Uuid,
    activated_by: Uuid,
    activated_at: chrono::DateTime<chrono::Utc>,
}

#[derive(Debug, sqlx::FromRow)]
struct RouteTargetRow {
    id: Uuid,
    routing_id: Uuid,
    provider_model_id: Uuid,
    provider_id: Uuid,
    provider_name: String,
    provider_model: String,
    priority: i32,
    weight: i32,
    timeout_ms: i32,
    position: i32,
}

fn target_rows(rows: Vec<RouteTargetRow>) -> Result<Vec<RouteTargetRecord>, ConfigurationError> {
    Ok(rows
        .into_iter()
        .map(|row| RouteTargetRecord {
            id: row.id,
            routing_id: row.routing_id,
            provider_model_id: row.provider_model_id,
            provider_id: row.provider_id,
            provider_name: row.provider_name,
            upstream_model: row.provider_model,
            priority: row.priority,
            weight: row.weight,
            timeout_ms: row.timeout_ms,
            position: row.position,
        })
        .collect())
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
