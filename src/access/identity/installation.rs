use crate::database::error::Error;

pub async fn installation_id(pool: &sqlx::PgPool) -> Result<uuid::Uuid, Error> {
    Ok(
        sqlx::query_scalar::<_, uuid::Uuid>("SELECT id FROM installation_identity WHERE singleton")
            .fetch_one(pool)
            .await?,
    )
}

/// Operator-chosen name for this installation. `None` until first-run
/// setup creates the single installation row.
pub async fn installation_name(pool: &sqlx::PgPool) -> Result<Option<String>, Error> {
    Ok(sqlx::query_scalar::<_, String>(
        "SELECT installation_name FROM installation WHERE singleton",
    )
    .fetch_optional(pool)
    .await?)
}
