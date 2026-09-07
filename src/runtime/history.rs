use chrono::DateTime;
use chrono::Utc;
use uuid::Uuid;

use crate::database::cursor::Error;
use crate::database::cursor::Page;
use crate::database::cursor::checked_u64;
use crate::database::query::split_page;
use crate::database::reads::MAX_PAGE_SIZE;

#[derive(Clone, Debug)]
pub struct GenerationRecord {
    pub id: Uuid,
    pub sequence: u64,
    pub sha256_hex: String,
    pub created_by: Uuid,
    pub created_by_email: String,
    pub created_at: DateTime<Utc>,
}

pub async fn runtime_generations(
    pool: &sqlx::PgPool,
    before_sequence: Option<u64>,
    limit: u16,
) -> Result<Page<GenerationRecord>, Error> {
    let page_size = limit.clamp(1, MAX_PAGE_SIZE);
    let before = before_sequence
        .map(i64::try_from)
        .transpose()
        .map_err(|_| Error::InvalidCursor)?;
    let rows = sqlx::query_as::<_, RuntimeGenerationsRow>(
        "SELECT g.id, g.sequence, encode(g.release_sha256, 'hex') AS \"sha256_hex\", \
                    g.created_by, u.email AS created_by_email, g.created_at \
             FROM runtime_generations g JOIN users u ON u.id = g.created_by \
             WHERE ($1::bigint IS NULL OR g.sequence < $1) \
             ORDER BY g.sequence DESC LIMIT $2",
    )
    .bind(before)
    .bind(i64::from(page_size) + 1)
    .fetch_all(pool)
    .await?;
    let items = rows
        .into_iter()
        .map(|row| {
            Ok(GenerationRecord {
                id: row.id,
                sequence: checked_u64(row.sequence, "generation sequence")?,
                sha256_hex: row.sha256_hex,
                created_by: row.created_by,
                created_by_email: row.created_by_email,
                created_at: row.created_at,
            })
        })
        .collect::<Result<Vec<_>, Error>>()?;
    let (items, next_cursor) = split_page(items, usize::from(page_size), |item| {
        item.sequence.to_string()
    });
    Ok(Page { items, next_cursor })
}

#[derive(sqlx::FromRow)]
struct RuntimeGenerationsRow {
    id: uuid::Uuid,
    sequence: i64,
    sha256_hex: String,
    created_by: uuid::Uuid,
    created_by_email: String,
    created_at: chrono::DateTime<chrono::Utc>,
}
