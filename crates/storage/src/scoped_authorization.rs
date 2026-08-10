use sqlx::{Postgres, Transaction};
use uuid::Uuid;

async fn can_access_existing_api_key(
    transaction: &mut Transaction<'_, Postgres>,
    actor: Uuid,
    key_id: Uuid,
    allow_viewer: bool,
) -> Result<bool, sqlx::Error> {
    sqlx::query_scalar!(
        "SELECT EXISTS ( \
           SELECT 1 FROM api_keys key JOIN users actor ON actor.id = $1 \
            WHERE key.id = $2 AND actor.active AND ( \
              actor.role IN ('owner', 'operator') OR ($3 AND actor.role = 'viewer') OR \
              (actor.role = 'developer' AND ( \
                (key.owner_user_id = actor.id \
                  AND (key.team_id IS NULL OR EXISTS ( \
                    SELECT 1 FROM team_memberships membership \
                     WHERE membership.team_id = key.team_id \
                       AND membership.user_id = actor.id)) \
                  AND (key.project_id IS NULL OR EXISTS ( \
                    SELECT 1 FROM project_memberships membership \
                     WHERE membership.project_id = key.project_id \
                       AND membership.user_id = actor.id))) OR \
                (key.team_id IS NOT NULL AND EXISTS ( \
                   SELECT 1 FROM team_memberships membership \
                    WHERE membership.team_id = key.team_id \
                      AND membership.user_id = actor.id AND membership.role = 'admin')) OR \
                (key.project_id IS NOT NULL AND EXISTS ( \
                   SELECT 1 FROM project_memberships membership \
                    WHERE membership.project_id = key.project_id \
                      AND membership.user_id = actor.id AND membership.role = 'admin')) \
              )) \
            ) \
         ) AS \"value!\"",
        actor,
        key_id,
        allow_viewer
    )
    .fetch_one(&mut **transaction)
    .await
}

pub(crate) async fn can_manage_existing_api_key(
    transaction: &mut Transaction<'_, Postgres>,
    actor: Uuid,
    key_id: Uuid,
) -> Result<bool, sqlx::Error> {
    can_access_existing_api_key(transaction, actor, key_id, false).await
}

pub(crate) async fn can_manage_api_key_scope(
    transaction: &mut Transaction<'_, Postgres>,
    actor: Uuid,
    owner_user_id: Option<Uuid>,
    team_id: Option<Uuid>,
    project_id: Option<Uuid>,
) -> Result<bool, sqlx::Error> {
    sqlx::query_scalar!(
        "SELECT EXISTS ( \
           SELECT 1 FROM users actor WHERE actor.id = $1 AND actor.active AND ( \
             actor.role IN ('owner', 'operator') OR \
             (actor.role = 'developer' AND ( \
               ($2 = actor.id AND ( \
                  $3::uuid IS NULL OR EXISTS (SELECT 1 FROM team_memberships membership \
                    WHERE membership.team_id = $3 AND membership.user_id = actor.id)) \
                  AND ($4::uuid IS NULL OR EXISTS (SELECT 1 FROM project_memberships membership \
                    WHERE membership.project_id = $4 AND membership.user_id = actor.id))) OR \
               ($3::uuid IS NOT NULL AND EXISTS (SELECT 1 FROM team_memberships membership \
                    WHERE membership.team_id = $3 AND membership.user_id = actor.id \
                      AND membership.role = 'admin')) OR \
               ($4::uuid IS NOT NULL AND EXISTS (SELECT 1 FROM project_memberships membership \
                    WHERE membership.project_id = $4 AND membership.user_id = actor.id \
                      AND membership.role = 'admin')) \
             )) \
           ) \
         ) AS \"value!\"",
        actor,
        owner_user_id,
        team_id,
        project_id
    )
    .fetch_one(&mut **transaction)
    .await
}

pub(crate) async fn can_read_existing_api_key(
    transaction: &mut Transaction<'_, Postgres>,
    actor: Uuid,
    key_id: Uuid,
) -> Result<bool, sqlx::Error> {
    can_access_existing_api_key(transaction, actor, key_id, true).await
}

pub(crate) async fn is_installation_admin(
    transaction: &mut Transaction<'_, Postgres>,
    actor: Uuid,
) -> Result<bool, sqlx::Error> {
    sqlx::query_scalar!(
        "SELECT EXISTS (SELECT 1 FROM users WHERE id = $1 AND active \
          AND role IN ('owner', 'operator')) AS \"value!\"",
        actor
    )
    .fetch_one(&mut **transaction)
    .await
}

pub(crate) async fn can_manage_team(
    transaction: &mut Transaction<'_, Postgres>,
    actor: Uuid,
    team_id: Uuid,
) -> Result<bool, sqlx::Error> {
    sqlx::query_scalar!(
        "SELECT EXISTS (SELECT 1 FROM users actor WHERE actor.id = $1 AND actor.active AND ( \
           actor.role IN ('owner', 'operator') OR \
           (actor.role = 'developer' AND EXISTS (SELECT 1 FROM team_memberships membership \
             WHERE membership.team_id = $2 AND membership.user_id = actor.id \
               AND membership.role = 'admin')))) AS \"value!\"",
        actor,
        team_id
    )
    .fetch_one(&mut **transaction)
    .await
}

pub(crate) async fn can_manage_project(
    transaction: &mut Transaction<'_, Postgres>,
    actor: Uuid,
    project_id: Uuid,
) -> Result<bool, sqlx::Error> {
    sqlx::query_scalar!(
        "SELECT EXISTS (SELECT 1 FROM projects project JOIN users actor ON actor.id = $1 \
          WHERE project.id = $2 AND actor.active AND (actor.role IN ('owner', 'operator') OR \
            (actor.role = 'developer' AND ( \
              EXISTS (SELECT 1 FROM team_memberships membership \
                WHERE membership.team_id = project.team_id AND membership.user_id = actor.id \
                  AND membership.role = 'admin') OR \
              EXISTS (SELECT 1 FROM project_memberships membership \
                WHERE membership.project_id = project.id AND membership.user_id = actor.id \
                  AND membership.role = 'admin'))))) AS \"value!\"",
        actor,
        project_id
    )
    .fetch_one(&mut **transaction)
    .await
}
