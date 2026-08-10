use chrono::Utc;
use olp_domain::ScopedRole;
use sqlx::{Postgres, Transaction};
use uuid::Uuid;

use super::{
    IdentityError, NewProject, NewServiceAccount, NewTeam, ProjectRecord, RuntimeGenerationRecord,
    ScopedMembershipRecord, ServiceAccountRecord, TeamRecord, insert_audit,
};
use crate::{
    PersistenceError, PgStore,
    idempotency::{
        IdempotencyOutcome, IdempotencyResponse, ReplayableIdempotency, ReplayableIdempotencyClaim,
        claim_idempotency, claim_replayable_idempotency, complete_idempotency,
        complete_replayable_idempotency,
    },
    runtime::{compile_and_publish_runtime_in_transaction, prepare_runtime_mutation},
    scoped_authorization::{can_manage_project, can_manage_team, is_installation_admin},
};

#[derive(Debug)]
pub struct ScopedResourceUpdate<T> {
    pub resource: T,
    pub runtime_generation: Option<RuntimeGenerationRecord>,
}

impl PgStore {
    pub async fn list_teams(
        &self,
        actor: Uuid,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<Vec<TeamRecord>, IdentityError> {
        Ok(sqlx::query_as!(
            TeamRecord,
            "SELECT team.id, team.name, team.active, team.etag, team.created_by, \
                    team.created_at, team.updated_at \
               FROM teams team JOIN users actor ON actor.id = $1 \
              WHERE actor.active AND ($2::uuid IS NULL OR team.id > $2) AND ( \
                actor.role IN ('owner', 'operator', 'viewer') OR \
                EXISTS (SELECT 1 FROM team_memberships membership \
                  WHERE membership.team_id = team.id AND membership.user_id = actor.id)) \
              ORDER BY team.id LIMIT $3",
            actor,
            cursor,
            limit
        )
        .fetch_all(self.pool())
        .await?)
    }

    pub async fn get_team(&self, actor: Uuid, id: Uuid) -> Result<TeamRecord, IdentityError> {
        sqlx::query_as!(
            TeamRecord,
            "SELECT team.id, team.name, team.active, team.etag, team.created_by, \
                    team.created_at, team.updated_at \
               FROM teams team JOIN users actor ON actor.id = $1 \
              WHERE team.id = $2 AND actor.active AND ( \
                actor.role IN ('owner', 'operator', 'viewer') OR \
                EXISTS (SELECT 1 FROM team_memberships membership \
                  WHERE membership.team_id = team.id AND membership.user_id = actor.id))",
            actor,
            id
        )
        .fetch_optional(self.pool())
        .await?
        .ok_or(IdentityError::NotFound)
    }

    pub async fn create_team<F>(
        &self,
        input: NewTeam,
        replay: ReplayableIdempotency<'_>,
        build_response: F,
    ) -> Result<IdempotencyOutcome<TeamRecord>, IdentityError>
    where
        F: FnOnce(&TeamRecord) -> Result<IdempotencyResponse, PersistenceError>,
    {
        let name = valid_name(&input.name)?;
        let mut transaction = self.pool().begin().await?;
        if !is_installation_admin(&mut transaction, input.actor).await? {
            return Err(IdentityError::Forbidden);
        }
        let replayed = claim_replay(
            &mut transaction,
            input.actor,
            "team.create",
            &input.idempotency_key,
            replay,
        )
        .await?;
        if let Some(response) = replayed {
            transaction.rollback().await?;
            return Ok(IdempotencyOutcome::Replayed(response));
        }
        if !is_installation_admin(&mut transaction, input.actor).await? {
            return Err(IdentityError::Forbidden);
        }
        let now = Utc::now();
        let id = Uuid::now_v7();
        let etag = Uuid::now_v7();
        let record = sqlx::query_as!(
            TeamRecord,
            "INSERT INTO teams (id, name, etag, created_by, created_at, updated_at) \
             VALUES ($1, $2, $3, $4, $5, $5) \
             RETURNING id, name, active, etag, created_by, created_at, updated_at",
            id,
            name,
            etag,
            input.actor,
            now
        )
        .fetch_one(&mut *transaction)
        .await?;
        sqlx::query!(
            "INSERT INTO team_memberships \
             (team_id, user_id, role, etag, created_by, created_at, updated_at) \
             VALUES ($1, $2, 'admin', $3, $2, $4, $4)",
            id,
            input.actor,
            Uuid::now_v7(),
            now
        )
        .execute(&mut *transaction)
        .await?;
        insert_audit(
            &mut transaction,
            input.actor,
            "team.create",
            "team",
            &id.to_string(),
        )
        .await?;
        let response = build_response(&record)?;
        finish_replay(
            &mut transaction,
            input.actor,
            "team.create",
            &input.idempotency_key,
            replay,
            &response,
        )
        .await?;
        transaction.commit().await?;
        Ok(IdempotencyOutcome::Executed {
            value: record,
            response,
        })
    }

    pub async fn list_projects(
        &self,
        actor: Uuid,
        team_id: Option<Uuid>,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<Vec<ProjectRecord>, IdentityError> {
        Ok(sqlx::query_as!(
            ProjectRecord,
            "SELECT project.id, project.team_id, project.name, project.active, project.etag, \
                    project.created_by, project.created_at, project.updated_at \
               FROM projects project JOIN users actor ON actor.id = $1 \
              WHERE actor.active AND ($2::uuid IS NULL OR project.team_id = $2) \
                AND ($3::uuid IS NULL OR project.id > $3) AND ( \
                  actor.role IN ('owner', 'operator', 'viewer') OR \
                  EXISTS (SELECT 1 FROM team_memberships membership \
                    WHERE membership.team_id = project.team_id AND membership.user_id = actor.id) OR \
                  EXISTS (SELECT 1 FROM project_memberships membership \
                    WHERE membership.project_id = project.id AND membership.user_id = actor.id)) \
              ORDER BY project.id LIMIT $4",
            actor,
            team_id,
            cursor,
            limit
        )
        .fetch_all(self.pool())
        .await?)
    }

    pub async fn get_project(&self, actor: Uuid, id: Uuid) -> Result<ProjectRecord, IdentityError> {
        sqlx::query_as!(
            ProjectRecord,
            "SELECT project.id, project.team_id, project.name, project.active, project.etag, \
                    project.created_by, project.created_at, project.updated_at \
               FROM projects project JOIN users actor ON actor.id = $1 \
              WHERE project.id = $2 AND actor.active AND ( \
                actor.role IN ('owner', 'operator', 'viewer') OR \
                EXISTS (SELECT 1 FROM team_memberships membership \
                  WHERE membership.team_id = project.team_id AND membership.user_id = actor.id) OR \
                EXISTS (SELECT 1 FROM project_memberships membership \
                  WHERE membership.project_id = project.id AND membership.user_id = actor.id))",
            actor,
            id
        )
        .fetch_optional(self.pool())
        .await?
        .ok_or(IdentityError::NotFound)
    }

    pub async fn create_project<F>(
        &self,
        input: NewProject,
        replay: ReplayableIdempotency<'_>,
        build_response: F,
    ) -> Result<IdempotencyOutcome<ProjectRecord>, IdentityError>
    where
        F: FnOnce(&ProjectRecord) -> Result<IdempotencyResponse, PersistenceError>,
    {
        let name = valid_name(&input.name)?;
        let mut transaction = self.pool().begin().await?;
        if !can_manage_team(&mut transaction, input.actor, input.team_id).await? {
            return Err(IdentityError::Forbidden);
        }
        let replayed = claim_replay(
            &mut transaction,
            input.actor,
            "project.create",
            &input.idempotency_key,
            replay,
        )
        .await?;
        if let Some(response) = replayed {
            transaction.rollback().await?;
            return Ok(IdempotencyOutcome::Replayed(response));
        }
        if !can_manage_team(&mut transaction, input.actor, input.team_id).await? {
            return Err(IdentityError::Forbidden);
        }
        let now = Utc::now();
        let id = Uuid::now_v7();
        let record = sqlx::query_as!(
            ProjectRecord,
            "INSERT INTO projects \
             (id, team_id, name, etag, created_by, created_at, updated_at) \
             SELECT $1, team.id, $3, $4, $5, $6, $6 FROM teams team \
              WHERE team.id = $2 AND team.active \
             RETURNING id, team_id, name, active, etag, created_by, created_at, updated_at",
            id,
            input.team_id,
            name,
            Uuid::now_v7(),
            input.actor,
            now
        )
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or_else(|| IdentityError::Invalid("team must be active".to_owned()))?;
        sqlx::query!(
            "INSERT INTO project_memberships \
             (project_id, team_id, user_id, role, etag, created_by, created_at, updated_at) \
             SELECT $1, $2, $3, 'admin', $4, $3, $5, $5 \
              WHERE EXISTS (SELECT 1 FROM team_memberships \
                WHERE team_id = $2 AND user_id = $3) ON CONFLICT DO NOTHING",
            id,
            input.team_id,
            input.actor,
            Uuid::now_v7(),
            now
        )
        .execute(&mut *transaction)
        .await?;
        insert_audit(
            &mut transaction,
            input.actor,
            "project.create",
            "project",
            &id.to_string(),
        )
        .await?;
        let response = build_response(&record)?;
        finish_replay(
            &mut transaction,
            input.actor,
            "project.create",
            &input.idempotency_key,
            replay,
            &response,
        )
        .await?;
        transaction.commit().await?;
        Ok(IdempotencyOutcome::Executed {
            value: record,
            response,
        })
    }

    pub async fn list_service_accounts(
        &self,
        actor: Uuid,
        project_id: Option<Uuid>,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<Vec<ServiceAccountRecord>, IdentityError> {
        Ok(sqlx::query_as!(
            ServiceAccountRecord,
            "SELECT account.id, account.team_id, account.project_id, account.name, account.active, \
                    account.etag, account.created_by, account.created_at, account.updated_at \
               FROM service_accounts account JOIN users actor ON actor.id = $1 \
              WHERE actor.active AND ($2::uuid IS NULL OR account.project_id = $2) \
                AND ($3::uuid IS NULL OR account.id > $3) AND ( \
                  actor.role IN ('owner', 'operator', 'viewer') OR \
                  EXISTS (SELECT 1 FROM team_memberships membership \
                    WHERE membership.team_id = account.team_id AND membership.user_id = actor.id) OR \
                  EXISTS (SELECT 1 FROM project_memberships membership \
                    WHERE membership.project_id = account.project_id AND membership.user_id = actor.id)) \
              ORDER BY account.id LIMIT $4",
            actor,
            project_id,
            cursor,
            limit
        )
        .fetch_all(self.pool())
        .await?)
    }

    pub async fn get_service_account(
        &self,
        actor: Uuid,
        id: Uuid,
    ) -> Result<ServiceAccountRecord, IdentityError> {
        sqlx::query_as!(
            ServiceAccountRecord,
            "SELECT account.id, account.team_id, account.project_id, account.name, account.active, \
                    account.etag, account.created_by, account.created_at, account.updated_at \
               FROM service_accounts account JOIN users actor ON actor.id = $1 \
              WHERE account.id = $2 AND actor.active AND ( \
                actor.role IN ('owner', 'operator', 'viewer') OR \
                EXISTS (SELECT 1 FROM team_memberships membership \
                  WHERE membership.team_id = account.team_id AND membership.user_id = actor.id) OR \
                EXISTS (SELECT 1 FROM project_memberships membership \
                  WHERE membership.project_id = account.project_id AND membership.user_id = actor.id))",
            actor,
            id
        )
        .fetch_optional(self.pool())
        .await?
        .ok_or(IdentityError::NotFound)
    }

    pub async fn create_service_account<F>(
        &self,
        input: NewServiceAccount,
        replay: ReplayableIdempotency<'_>,
        build_response: F,
    ) -> Result<IdempotencyOutcome<ServiceAccountRecord>, IdentityError>
    where
        F: FnOnce(&ServiceAccountRecord) -> Result<IdempotencyResponse, PersistenceError>,
    {
        let name = valid_name(&input.name)?;
        let mut transaction = self.pool().begin().await?;
        if !can_manage_project(&mut transaction, input.actor, input.project_id).await? {
            return Err(IdentityError::Forbidden);
        }
        let replayed = claim_replay(
            &mut transaction,
            input.actor,
            "service_account.create",
            &input.idempotency_key,
            replay,
        )
        .await?;
        if let Some(response) = replayed {
            transaction.rollback().await?;
            return Ok(IdempotencyOutcome::Replayed(response));
        }
        if !can_manage_project(&mut transaction, input.actor, input.project_id).await? {
            return Err(IdentityError::Forbidden);
        }
        let now = Utc::now();
        let id = Uuid::now_v7();
        let record = sqlx::query_as!(
            ServiceAccountRecord,
            "INSERT INTO service_accounts \
             (id, team_id, project_id, name, etag, created_by, created_at, updated_at) \
             SELECT $1, project.team_id, project.id, $4, $5, $6, $7, $7 \
               FROM projects project JOIN teams team ON team.id = project.team_id \
              WHERE project.id = $3 AND project.team_id = $2 AND project.active AND team.active \
             RETURNING id, team_id, project_id, name, active, etag, created_by, created_at, updated_at",
            id,
            input.team_id,
            input.project_id,
            name,
            Uuid::now_v7(),
            input.actor,
            now
        )
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or_else(|| IdentityError::Invalid("project and team must be active".to_owned()))?;
        insert_audit(
            &mut transaction,
            input.actor,
            "service_account.create",
            "service_account",
            &id.to_string(),
        )
        .await?;
        let response = build_response(&record)?;
        finish_replay(
            &mut transaction,
            input.actor,
            "service_account.create",
            &input.idempotency_key,
            replay,
            &response,
        )
        .await?;
        transaction.commit().await?;
        Ok(IdempotencyOutcome::Executed {
            value: record,
            response,
        })
    }

    pub async fn update_team(
        &self,
        id: Uuid,
        name: Option<&str>,
        active: Option<bool>,
        expected_etag: Uuid,
        actor: Uuid,
        idempotency_key: &str,
    ) -> Result<ScopedResourceUpdate<TeamRecord>, IdentityError> {
        if name.is_none() && active.is_none() {
            return Err(IdentityError::Invalid(
                "name or active status is required".to_owned(),
            ));
        }
        let name = name.map(valid_name).transpose()?;
        let mut transaction = self
            .pool()
            .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
            .await?;
        prepare_runtime_mutation(&mut transaction).await?;
        claim_mutation(&mut transaction, actor, "team.update", idempotency_key).await?;
        if !can_manage_team(&mut transaction, actor, id).await? {
            return Err(IdentityError::NotFound);
        }
        let record = sqlx::query_as!(
            TeamRecord,
            "UPDATE teams SET name = COALESCE($2, name), active = COALESCE($3, active), \
                    etag = $4, updated_at = now() WHERE id = $1 AND etag = $5 \
             RETURNING id, name, active, etag, created_by, created_at, updated_at",
            id,
            name,
            active,
            Uuid::now_v7(),
            expected_etag
        )
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or_else(|| IdentityError::PreconditionFailed)?;
        let generation = if active == Some(false) {
            sqlx::query!(
                "UPDATE projects SET active = false, etag = uuidv7(), updated_at = now() \
                 WHERE team_id = $1 AND active",
                id
            )
            .execute(&mut *transaction)
            .await?;
            sqlx::query!(
                "UPDATE service_accounts SET active = false, etag = uuidv7(), updated_at = now() \
                 WHERE team_id = $1 AND active",
                id
            )
            .execute(&mut *transaction)
            .await?;
            revoke_scoped_keys(&mut transaction, Some(id), None, None).await?;
            Some(publish_runtime(&mut transaction, actor).await?)
        } else {
            None
        };
        insert_audit(
            &mut transaction,
            actor,
            "team.update",
            "team",
            &id.to_string(),
        )
        .await?;
        complete_idempotency(
            &mut transaction,
            actor,
            "team.update",
            idempotency_key,
            &id.to_string(),
        )
        .await?;
        transaction.commit().await?;
        Ok(ScopedResourceUpdate {
            resource: record,
            runtime_generation: generation,
        })
    }

    pub async fn update_project(
        &self,
        id: Uuid,
        name: Option<&str>,
        active: Option<bool>,
        expected_etag: Uuid,
        actor: Uuid,
        idempotency_key: &str,
    ) -> Result<ScopedResourceUpdate<ProjectRecord>, IdentityError> {
        if name.is_none() && active.is_none() {
            return Err(IdentityError::Invalid(
                "name or active status is required".to_owned(),
            ));
        }
        let name = name.map(valid_name).transpose()?;
        let mut transaction = self
            .pool()
            .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
            .await?;
        prepare_runtime_mutation(&mut transaction).await?;
        claim_mutation(&mut transaction, actor, "project.update", idempotency_key).await?;
        if !can_manage_project(&mut transaction, actor, id).await? {
            return Err(IdentityError::NotFound);
        }
        let record = sqlx::query_as!(
            ProjectRecord,
            "UPDATE projects project SET name = COALESCE($2, project.name), \
                    active = COALESCE($3, project.active), etag = $4, updated_at = now() \
              FROM teams team WHERE project.id = $1 AND project.etag = $5 \
                AND team.id = project.team_id AND ($3::boolean IS DISTINCT FROM true OR team.active) \
             RETURNING project.id, project.team_id, project.name, project.active, project.etag, \
                       project.created_by, project.created_at, project.updated_at",
            id,
            name,
            active,
            Uuid::now_v7(),
            expected_etag
        )
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(IdentityError::PreconditionFailed)?;
        let generation = if active == Some(false) {
            sqlx::query!(
                "UPDATE service_accounts SET active = false, etag = uuidv7(), updated_at = now() \
                 WHERE project_id = $1 AND active",
                id
            )
            .execute(&mut *transaction)
            .await?;
            revoke_scoped_keys(&mut transaction, None, Some(id), None).await?;
            Some(publish_runtime(&mut transaction, actor).await?)
        } else {
            None
        };
        insert_audit(
            &mut transaction,
            actor,
            "project.update",
            "project",
            &id.to_string(),
        )
        .await?;
        complete_idempotency(
            &mut transaction,
            actor,
            "project.update",
            idempotency_key,
            &id.to_string(),
        )
        .await?;
        transaction.commit().await?;
        Ok(ScopedResourceUpdate {
            resource: record,
            runtime_generation: generation,
        })
    }

    pub async fn update_service_account(
        &self,
        id: Uuid,
        name: Option<&str>,
        active: Option<bool>,
        expected_etag: Uuid,
        actor: Uuid,
        idempotency_key: &str,
    ) -> Result<ScopedResourceUpdate<ServiceAccountRecord>, IdentityError> {
        if name.is_none() && active.is_none() {
            return Err(IdentityError::Invalid(
                "name or active status is required".to_owned(),
            ));
        }
        let name = name.map(valid_name).transpose()?;
        let mut transaction = self
            .pool()
            .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
            .await?;
        prepare_runtime_mutation(&mut transaction).await?;
        claim_mutation(
            &mut transaction,
            actor,
            "service_account.update",
            idempotency_key,
        )
        .await?;
        let project_id =
            sqlx::query_scalar!("SELECT project_id FROM service_accounts WHERE id = $1", id)
                .fetch_optional(&mut *transaction)
                .await?
                .ok_or(IdentityError::NotFound)?;
        if !can_manage_project(&mut transaction, actor, project_id).await? {
            return Err(IdentityError::NotFound);
        }
        let record = sqlx::query_as!(
            ServiceAccountRecord,
            "UPDATE service_accounts account SET name = COALESCE($2, account.name), \
                    active = COALESCE($3, account.active), etag = $4, updated_at = now() \
              FROM projects project JOIN teams team ON team.id = project.team_id \
             WHERE account.id = $1 AND account.etag = $5 AND project.id = account.project_id \
               AND ($3::boolean IS DISTINCT FROM true OR (project.active AND team.active)) \
             RETURNING account.id, account.team_id, account.project_id, account.name, account.active, \
                       account.etag, account.created_by, account.created_at, account.updated_at",
            id,
            name,
            active,
            Uuid::now_v7(),
            expected_etag
        )
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(IdentityError::PreconditionFailed)?;
        let generation = if active == Some(false) {
            revoke_scoped_keys(&mut transaction, None, None, Some(id)).await?;
            Some(publish_runtime(&mut transaction, actor).await?)
        } else {
            None
        };
        insert_audit(
            &mut transaction,
            actor,
            "service_account.update",
            "service_account",
            &id.to_string(),
        )
        .await?;
        complete_idempotency(
            &mut transaction,
            actor,
            "service_account.update",
            idempotency_key,
            &id.to_string(),
        )
        .await?;
        transaction.commit().await?;
        Ok(ScopedResourceUpdate {
            resource: record,
            runtime_generation: generation,
        })
    }

    pub async fn put_team_membership(
        &self,
        team_id: Uuid,
        user_id: Uuid,
        role: ScopedRole,
        expected_etag: Option<Uuid>,
        actor: Uuid,
        idempotency_key: &str,
    ) -> Result<ScopedMembershipRecord, IdentityError> {
        let mut transaction = self.pool().begin().await?;
        claim_mutation(
            &mut transaction,
            actor,
            "team_membership.put",
            idempotency_key,
        )
        .await?;
        if !can_manage_team(&mut transaction, actor, team_id).await? {
            return Err(IdentityError::NotFound);
        }
        let row = sqlx::query!(
            "WITH valid_scope AS ( \
                 SELECT team.id AS team_id, account.id AS user_id \
                   FROM teams team JOIN users account ON account.id = $2 \
                  WHERE team.id = $1 AND team.active AND account.active \
             ), updated AS ( \
                 UPDATE team_memberships membership \
                    SET role = $3::text::scoped_membership_role, etag = $4, updated_at = now() \
                   FROM valid_scope \
                  WHERE membership.team_id = valid_scope.team_id \
                    AND membership.user_id = valid_scope.user_id \
                    AND $6::uuid IS NOT NULL AND membership.etag = $6 \
                 RETURNING membership.team_id, membership.user_id, membership.role, \
                           membership.etag, membership.created_by, membership.created_at, \
                           membership.updated_at \
             ), inserted AS ( \
                 INSERT INTO team_memberships \
                    (team_id, user_id, role, etag, created_by, created_at, updated_at) \
                 SELECT team_id, user_id, $3::text::scoped_membership_role, $4, $5, now(), now() \
                   FROM valid_scope WHERE $6::uuid IS NULL \
                 ON CONFLICT (team_id, user_id) DO NOTHING \
                 RETURNING team_id, user_id, role, etag, created_by, created_at, updated_at \
             ) \
             SELECT team_id AS \"team_id!\", user_id AS \"user_id!\", \
                    role::text AS \"role!\", etag AS \"etag!\", created_by AS \"created_by!\", \
                    created_at AS \"created_at!\", updated_at AS \"updated_at!\" FROM updated \
             UNION ALL \
             SELECT team_id, user_id, role::text AS \"role!\", etag, created_by, \
                    created_at, updated_at FROM inserted",
            team_id,
            user_id,
            role.as_str(),
            Uuid::now_v7(),
            actor,
            expected_etag
        )
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(IdentityError::PreconditionFailed)?;
        let record = membership_from_row(MembershipRow {
            team_id: row.team_id,
            project_id: None,
            user_id: row.user_id,
            role: row.role,
            etag: row.etag,
            created_by: row.created_by,
            created_at: row.created_at,
            updated_at: row.updated_at,
        })?;
        insert_audit(
            &mut transaction,
            actor,
            "team_membership.put",
            "team_membership",
            &format!("{team_id}:{user_id}"),
        )
        .await?;
        complete_idempotency(
            &mut transaction,
            actor,
            "team_membership.put",
            idempotency_key,
            &format!("{team_id}:{user_id}"),
        )
        .await?;
        transaction.commit().await?;
        Ok(record)
    }

    pub async fn put_project_membership(
        &self,
        project_id: Uuid,
        user_id: Uuid,
        role: ScopedRole,
        expected_etag: Option<Uuid>,
        actor: Uuid,
        idempotency_key: &str,
    ) -> Result<ScopedMembershipRecord, IdentityError> {
        let mut transaction = self.pool().begin().await?;
        claim_mutation(
            &mut transaction,
            actor,
            "project_membership.put",
            idempotency_key,
        )
        .await?;
        if !can_manage_project(&mut transaction, actor, project_id).await? {
            return Err(IdentityError::NotFound);
        }
        let row = sqlx::query!(
            "WITH valid_scope AS ( \
                 SELECT project.id AS project_id, project.team_id, account.id AS user_id \
                   FROM projects project \
                   JOIN users account ON account.id = $2 \
                   JOIN team_memberships team_member ON team_member.team_id = project.team_id \
                    AND team_member.user_id = account.id \
                  WHERE project.id = $1 AND project.active AND account.active \
             ), updated AS ( \
                 UPDATE project_memberships membership \
                    SET role = $3::text::scoped_membership_role, etag = $4, updated_at = now() \
                   FROM valid_scope \
                  WHERE membership.project_id = valid_scope.project_id \
                    AND membership.user_id = valid_scope.user_id \
                    AND $6::uuid IS NOT NULL AND membership.etag = $6 \
                 RETURNING membership.project_id, membership.team_id, membership.user_id, \
                           membership.role, membership.etag, membership.created_by, \
                           membership.created_at, membership.updated_at \
             ), inserted AS ( \
                 INSERT INTO project_memberships \
                    (project_id, team_id, user_id, role, etag, created_by, created_at, updated_at) \
                 SELECT project_id, team_id, user_id, $3::text::scoped_membership_role, \
                        $4, $5, now(), now() FROM valid_scope WHERE $6::uuid IS NULL \
                 ON CONFLICT (project_id, user_id) DO NOTHING \
                 RETURNING project_id, team_id, user_id, role, etag, created_by, created_at, \
                           updated_at \
             ) \
             SELECT project_id AS \"project_id!\", team_id AS \"team_id!\", \
                    user_id AS \"user_id!\", role::text AS \"role!\", etag AS \"etag!\", \
                    created_by AS \"created_by!\", created_at AS \"created_at!\", \
                    updated_at AS \"updated_at!\" FROM updated \
             UNION ALL \
             SELECT project_id, team_id, user_id, role::text AS \"role!\", etag, created_by, \
                    created_at, updated_at FROM inserted",
            project_id,
            user_id,
            role.as_str(),
            Uuid::now_v7(),
            actor,
            expected_etag
        )
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(IdentityError::PreconditionFailed)?;
        let record = membership_from_row(MembershipRow {
            team_id: row.team_id,
            project_id: Some(row.project_id),
            user_id: row.user_id,
            role: row.role,
            etag: row.etag,
            created_by: row.created_by,
            created_at: row.created_at,
            updated_at: row.updated_at,
        })?;
        insert_audit(
            &mut transaction,
            actor,
            "project_membership.put",
            "project_membership",
            &format!("{project_id}:{user_id}"),
        )
        .await?;
        complete_idempotency(
            &mut transaction,
            actor,
            "project_membership.put",
            idempotency_key,
            &format!("{project_id}:{user_id}"),
        )
        .await?;
        transaction.commit().await?;
        Ok(record)
    }

    pub async fn list_team_memberships(
        &self,
        actor: Uuid,
        team_id: Uuid,
    ) -> Result<Vec<ScopedMembershipRecord>, IdentityError> {
        self.get_team(actor, team_id).await?;
        let rows = sqlx::query!(
            "SELECT team_id, user_id, role::text AS \"role!\", etag, created_by, \
                    created_at, updated_at FROM team_memberships \
             WHERE team_id = $1 ORDER BY user_id",
            team_id
        )
        .fetch_all(self.pool())
        .await?;
        rows.into_iter()
            .map(|row| {
                membership_from_row(MembershipRow {
                    team_id: row.team_id,
                    project_id: None,
                    user_id: row.user_id,
                    role: row.role,
                    etag: row.etag,
                    created_by: row.created_by,
                    created_at: row.created_at,
                    updated_at: row.updated_at,
                })
            })
            .collect()
    }

    pub async fn list_project_memberships(
        &self,
        actor: Uuid,
        project_id: Uuid,
    ) -> Result<Vec<ScopedMembershipRecord>, IdentityError> {
        self.get_project(actor, project_id).await?;
        let rows = sqlx::query!(
            "SELECT project_id, team_id, user_id, role::text AS \"role!\", etag, created_by, \
                    created_at, updated_at FROM project_memberships \
             WHERE project_id = $1 ORDER BY user_id",
            project_id
        )
        .fetch_all(self.pool())
        .await?;
        rows.into_iter()
            .map(|row| {
                membership_from_row(MembershipRow {
                    team_id: row.team_id,
                    project_id: Some(row.project_id),
                    user_id: row.user_id,
                    role: row.role,
                    etag: row.etag,
                    created_by: row.created_by,
                    created_at: row.created_at,
                    updated_at: row.updated_at,
                })
            })
            .collect()
    }

    pub async fn remove_team_membership(
        &self,
        team_id: Uuid,
        user_id: Uuid,
        expected_etag: Uuid,
        actor: Uuid,
        idempotency_key: &str,
    ) -> Result<Option<RuntimeGenerationRecord>, IdentityError> {
        let mut transaction = self
            .pool()
            .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
            .await?;
        prepare_runtime_mutation(&mut transaction).await?;
        claim_mutation(
            &mut transaction,
            actor,
            "team_membership.remove",
            idempotency_key,
        )
        .await?;
        if !can_manage_team(&mut transaction, actor, team_id).await? {
            return Err(IdentityError::NotFound);
        }
        let has_projects = sqlx::query_scalar!(
            "SELECT EXISTS (SELECT 1 FROM project_memberships \
              WHERE team_id = $1 AND user_id = $2) AS \"value!\"",
            team_id,
            user_id
        )
        .fetch_one(&mut *transaction)
        .await?;
        if has_projects {
            return Err(IdentityError::Invalid(
                "remove project memberships before the team membership".to_owned(),
            ));
        }
        let deleted = sqlx::query!(
            "DELETE FROM team_memberships WHERE team_id = $1 AND user_id = $2 AND etag = $3",
            team_id,
            user_id,
            expected_etag
        )
        .execute(&mut *transaction)
        .await?
        .rows_affected();
        if deleted == 0 {
            return Err(IdentityError::PreconditionFailed);
        }
        let revoked = sqlx::query!(
            "UPDATE api_keys SET revoked_at = now(), etag = uuidv7() \
             WHERE owner_user_id = $1 AND team_id = $2 AND revoked_at IS NULL",
            user_id,
            team_id
        )
        .execute(&mut *transaction)
        .await?
        .rows_affected();
        let generation = publish_if_revoked(&mut transaction, actor, revoked).await?;
        insert_audit(
            &mut transaction,
            actor,
            "team_membership.remove",
            "team_membership",
            &format!("{team_id}:{user_id}"),
        )
        .await?;
        complete_idempotency(
            &mut transaction,
            actor,
            "team_membership.remove",
            idempotency_key,
            &format!("{team_id}:{user_id}"),
        )
        .await?;
        transaction.commit().await?;
        Ok(generation)
    }

    pub async fn remove_project_membership(
        &self,
        project_id: Uuid,
        user_id: Uuid,
        expected_etag: Uuid,
        actor: Uuid,
        idempotency_key: &str,
    ) -> Result<Option<RuntimeGenerationRecord>, IdentityError> {
        let mut transaction = self
            .pool()
            .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
            .await?;
        prepare_runtime_mutation(&mut transaction).await?;
        claim_mutation(
            &mut transaction,
            actor,
            "project_membership.remove",
            idempotency_key,
        )
        .await?;
        if !can_manage_project(&mut transaction, actor, project_id).await? {
            return Err(IdentityError::NotFound);
        }
        let deleted = sqlx::query!(
            "DELETE FROM project_memberships WHERE project_id = $1 AND user_id = $2 AND etag = $3",
            project_id,
            user_id,
            expected_etag
        )
        .execute(&mut *transaction)
        .await?
        .rows_affected();
        if deleted == 0 {
            return Err(IdentityError::PreconditionFailed);
        }
        let revoked = sqlx::query!(
            "UPDATE api_keys SET revoked_at = now(), etag = uuidv7() \
             WHERE owner_user_id = $1 AND project_id = $2 AND revoked_at IS NULL",
            user_id,
            project_id
        )
        .execute(&mut *transaction)
        .await?
        .rows_affected();
        let generation = publish_if_revoked(&mut transaction, actor, revoked).await?;
        insert_audit(
            &mut transaction,
            actor,
            "project_membership.remove",
            "project_membership",
            &format!("{project_id}:{user_id}"),
        )
        .await?;
        complete_idempotency(
            &mut transaction,
            actor,
            "project_membership.remove",
            idempotency_key,
            &format!("{project_id}:{user_id}"),
        )
        .await?;
        transaction.commit().await?;
        Ok(generation)
    }
}

fn valid_name(value: &str) -> Result<&str, IdentityError> {
    let value = value.trim();
    if value.is_empty() || value.chars().count() > 120 {
        return Err(IdentityError::Invalid(
            "name must contain between 1 and 120 characters".to_owned(),
        ));
    }
    Ok(value)
}

async fn claim_replay(
    transaction: &mut Transaction<'_, Postgres>,
    actor: Uuid,
    operation: &str,
    key: &str,
    replay: ReplayableIdempotency<'_>,
) -> Result<Option<IdempotencyResponse>, IdentityError> {
    match claim_replayable_idempotency(
        transaction,
        actor,
        operation,
        key,
        replay.request_fingerprint(),
        replay.master_key(),
    )
    .await?
    {
        ReplayableIdempotencyClaim::Execute => Ok(None),
        ReplayableIdempotencyClaim::Replay(response) => Ok(Some(response)),
        ReplayableIdempotencyClaim::Conflict => Err(IdentityError::IdempotencyConflict),
        ReplayableIdempotencyClaim::InProgress => Err(IdentityError::IdempotencyInProgress),
    }
}

async fn finish_replay(
    transaction: &mut Transaction<'_, Postgres>,
    actor: Uuid,
    operation: &str,
    key: &str,
    replay: ReplayableIdempotency<'_>,
    response: &IdempotencyResponse,
) -> Result<(), IdentityError> {
    complete_replayable_idempotency(
        transaction,
        actor,
        operation,
        key,
        replay.request_fingerprint(),
        replay.master_key(),
        response,
    )
    .await?;
    Ok(())
}

async fn claim_mutation(
    transaction: &mut Transaction<'_, Postgres>,
    actor: Uuid,
    operation: &str,
    key: &str,
) -> Result<(), IdentityError> {
    if !claim_idempotency(transaction, actor, operation, key).await? {
        return Err(IdentityError::IdempotencyConflict);
    }
    Ok(())
}

async fn revoke_scoped_keys(
    transaction: &mut Transaction<'_, Postgres>,
    team_id: Option<Uuid>,
    project_id: Option<Uuid>,
    service_account_id: Option<Uuid>,
) -> Result<u64, sqlx::Error> {
    Ok(sqlx::query!(
        "UPDATE api_keys SET revoked_at = now(), etag = uuidv7() \
         WHERE revoked_at IS NULL AND ($1::uuid IS NULL OR team_id = $1) \
           AND ($2::uuid IS NULL OR project_id = $2) \
           AND ($3::uuid IS NULL OR owner_service_account_id = $3)",
        team_id,
        project_id,
        service_account_id
    )
    .execute(&mut **transaction)
    .await?
    .rows_affected())
}

async fn publish_if_revoked(
    transaction: &mut Transaction<'_, Postgres>,
    actor: Uuid,
    revoked: u64,
) -> Result<Option<RuntimeGenerationRecord>, IdentityError> {
    if revoked == 0 {
        return Ok(None);
    }
    Ok(Some(publish_runtime(transaction, actor).await?))
}

async fn publish_runtime(
    transaction: &mut Transaction<'_, Postgres>,
    actor: Uuid,
) -> Result<RuntimeGenerationRecord, IdentityError> {
    let release = compile_and_publish_runtime_in_transaction(transaction, actor).await?;
    Ok(RuntimeGenerationRecord {
        id: release.generation_id,
        sequence: release.sequence,
    })
}

struct MembershipRow {
    team_id: Uuid,
    project_id: Option<Uuid>,
    user_id: Uuid,
    role: String,
    etag: Uuid,
    created_by: Uuid,
    created_at: chrono::DateTime<Utc>,
    updated_at: chrono::DateTime<Utc>,
}

fn membership_from_row(row: MembershipRow) -> Result<ScopedMembershipRecord, IdentityError> {
    let role = match row.role.as_str() {
        "admin" => ScopedRole::Admin,
        "member" => ScopedRole::Member,
        _ => return Err(IdentityError::CorruptIdentity),
    };
    Ok(ScopedMembershipRecord {
        team_id: row.team_id,
        project_id: row.project_id,
        user_id: row.user_id,
        role,
        etag: row.etag,
        created_by: row.created_by,
        created_at: row.created_at,
        updated_at: row.updated_at,
    })
}
