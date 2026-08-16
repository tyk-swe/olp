//! Direct reads of the run database.
//!
//! `docs/architecture.md` "Data-safety invariants" is a claim about what is
//! *stored*, not about what the API returns. Asserting it through the
//! management API would only prove that the reader hides the data; the only
//! honest check reads the tables the writer wrote.

use sqlx::{Connection as _, Row as _, postgres::PgConnection};

/// A table and column pair whose rendered row text contained the needle.
#[derive(Debug)]
pub(crate) struct Sighting {
    pub(crate) table: String,
    pub(crate) sample: String,
}

/// Every base table in the `public` schema, partitions included.
async fn tables(connection: &mut PgConnection) -> Result<Vec<String>, String> {
    let rows = sqlx::query(
        "SELECT quote_ident(table_schema) || '.' || quote_ident(table_name) AS name \
         FROM information_schema.tables \
         WHERE table_schema = 'public' AND table_type = 'BASE TABLE' \
         ORDER BY table_name",
    )
    .fetch_all(&mut *connection)
    .await
    .map_err(|error| format!("failed to list tables: {error}"))?;
    rows.into_iter()
        .map(|row| {
            row.try_get::<String, _>("name")
                .map_err(|error| format!("failed to decode a table name: {error}"))
        })
        .collect()
}

/// Searches every durable row for `needle`, returning one sighting per table.
///
/// Each row is converted to JSON and its individual string values are matched.
/// This avoids PostgreSQL's composite-text escaping hiding commas, quotes, or
/// backslashes in plaintext. A needle stored in `bytea` or encrypted data still
/// will not match; both are acceptable exclusions because the invariant is
/// specifically about plaintext leaking into durable records.
pub(crate) async fn rows_containing(
    database_url: &str,
    needle: &str,
) -> Result<Vec<Sighting>, String> {
    let mut connection = PgConnection::connect(database_url)
        .await
        .map_err(|error| format!("failed to connect to the run database: {error}"))?;

    let mut sightings = Vec::new();
    for table in tables(&mut connection).await? {
        // Table names come from information_schema and are already quoted by
        // quote_ident; the needle stays a bind parameter.
        let statement = format!(
            "SELECT left(string_values.json_value #>> '{{}}', 400) AS sample \
             FROM {table} AS t \
             CROSS JOIN LATERAL jsonb_path_query( \
                 to_jsonb(t), '$.** ? (@.type() == \"string\")' \
             ) AS string_values(json_value) \
             WHERE strpos(string_values.json_value #>> '{{}}', $1) > 0 LIMIT 1"
        );
        let found = sqlx::query(sqlx::AssertSqlSafe(statement))
            .bind(needle)
            .fetch_optional(&mut connection)
            .await
            .map_err(|error| format!("failed to scan {table}: {error}"))?;
        if let Some(row) = found {
            sightings.push(Sighting {
                table,
                sample: row.try_get::<String, _>("sample").unwrap_or_default(),
            });
        }
    }

    connection.close().await.ok();
    Ok(sightings)
}

/// Renders sightings for a failure message.
pub(crate) fn describe(sightings: &[Sighting]) -> String {
    sightings
        .iter()
        .map(|sighting| format!("  {}: {}", sighting.table, sighting.sample))
        .collect::<Vec<_>>()
        .join("\n")
}
