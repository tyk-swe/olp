use crate::protocols::canonical::identity::OperationKind;
use chrono::DateTime;
use chrono::Utc;
use sqlx::FromRow;
use sqlx::Postgres;
use sqlx::QueryBuilder;
use uuid::Uuid;

use crate::database::cursor::Page;
use crate::database::cursor::Timestamp;
use crate::database::query::split_page;
use crate::database::reads::MAX_PAGE_SIZE;

use crate::media::jobs::MediaJobError;
use crate::media::jobs::MediaJobFilters;
use crate::media::jobs::MediaJobLifecycle;
use crate::media::jobs::MediaJobOrder;
use crate::media::jobs::MediaJobRecord;
use crate::media::jobs::MediaJobState;

const MEDIA_JOB_SELECT: &str = "SELECT j.id, j.upstream_job_id, j.api_key_id, j.provider_id,
            p.name AS provider_name, j.provider_model, j.route_slug,
            j.operation, j.surface, j.state::text AS state, j.lifecycle_state,
            j.progress_percent::real AS progress_percent,
            j.content_available, j.expires_at, j.error_class,
            j.completed_at, j.last_polled_at, j.reconciliation_error, j.deleted_at,
            j.runtime_generation_id, j.provider_revision_id, j.reconciliation_claim_id,
            j.reconciliation_attempts, j.next_reconciliation_at,
            j.last_reconciliation_at, j.etag,
            j.created_at, j.updated_at
     FROM async_media_jobs j
     JOIN providers p ON p.id = j.provider_id";

pub async fn media_job(pool: &sqlx::PgPool, id: Uuid) -> Result<MediaJobRecord, MediaJobError> {
    let row = sqlx::query_as::<_, MediaJobRow>(
        "SELECT j.id, j.upstream_job_id, j.api_key_id, j.provider_id,
                    p.name AS provider_name, j.provider_model, j.route_slug,
                    j.operation, j.surface, j.state::text AS \"state\", j.lifecycle_state,
                    j.progress_percent::real AS \"progress_percent\",
                    j.content_available, j.expires_at, j.error_class,
                    j.completed_at, j.last_polled_at, j.reconciliation_error, j.deleted_at,
                    j.runtime_generation_id, j.provider_revision_id, j.reconciliation_claim_id,
                    j.reconciliation_attempts, j.next_reconciliation_at,
                    j.last_reconciliation_at, j.etag,
                    j.created_at, j.updated_at
             FROM async_media_jobs j
             JOIN providers p ON p.id = j.provider_id
             WHERE j.id = $1",
    )
    .bind(id)
    .fetch_optional(pool)
    .await?
    .ok_or(MediaJobError::NotFound)?;
    media_job_from_row(row)
}

pub async fn media_jobs(
    pool: &sqlx::PgPool,
    filters: &MediaJobFilters,
    cursor: Option<&Timestamp>,
    limit: u16,
) -> Result<Page<MediaJobRecord>, MediaJobError> {
    let limit = limit.clamp(1, MAX_PAGE_SIZE);
    let mut query = QueryBuilder::<Postgres>::new(MEDIA_JOB_SELECT);
    query.push(" WHERE TRUE");
    push_filters(&mut query, filters);
    if let Some(value) = cursor {
        query
            .push(" AND (j.created_at, j.id) < (")
            .push_bind(value.at)
            .push(", ")
            .push_bind(value.id)
            .push(")");
    }
    query
        .push(" ORDER BY j.created_at DESC, j.id DESC LIMIT ")
        .push_bind(i64::from(limit) + 1);
    let rows = query
        .build_query_as::<MediaJobRow>()
        .fetch_all(pool)
        .await?;
    let items = rows
        .into_iter()
        .map(media_job_from_row)
        .collect::<Result<Vec<_>, _>>()?;
    let (items, next_cursor) = split_page(items, usize::from(limit), |last| {
        Timestamp {
            at: last.created_at,
            id: last.id,
        }
        .encode()
    });
    Ok(Page { items, next_cursor })
}

/// Client-facing video pagination uses the last public OLP video ID as its
/// cursor, not the opaque management timestamp cursor.
pub async fn media_jobs_after_id(
    pool: &sqlx::PgPool,
    filters: &MediaJobFilters,
    after: Option<Uuid>,
    order: MediaJobOrder,
    limit: u16,
) -> Result<Page<MediaJobRecord>, MediaJobError> {
    let limit = limit.clamp(1, MAX_PAGE_SIZE);
    let position = if let Some(after) = after {
        let row = sqlx::query_as::<_, MediaJobsAfterIdRow>(
            "SELECT created_at, id FROM async_media_jobs
                 WHERE id = $1
                   AND ($2::uuid IS NULL OR api_key_id = $2)
                   AND (cardinality($3::text[]) = 0 OR route_slug = ANY($3::text[]))
                   AND ($4::text IS NULL OR operation = $4)
                   AND ($5::text IS NULL OR surface = $5)",
        )
        .bind(after)
        .bind(filters.api_key_id)
        .bind(&filters.route_slugs)
        .bind(filters.operation.map(OperationKind::as_str))
        .bind(
            filters
                .surface
                .map(crate::protocols::canonical::identity::Surface::as_str),
        )
        .fetch_optional(pool)
        .await?
        .ok_or_else(|| MediaJobError::Invalid("video cursor is invalid".to_owned()))?;
        Some((row.created_at, row.id))
    } else {
        None
    };
    let mut query = QueryBuilder::<Postgres>::new(MEDIA_JOB_SELECT);
    query.push(" WHERE j.lifecycle_state = 'active'");
    push_filters(&mut query, filters);
    if let Some((created_at, id)) = position {
        query.push(" AND (j.created_at, j.id) ");
        match order {
            MediaJobOrder::Ascending => query.push(">"),
            MediaJobOrder::Descending => query.push("<"),
        };
        query
            .push(" (")
            .push_bind(created_at)
            .push(", ")
            .push_bind(id)
            .push(")");
    }
    match order {
        MediaJobOrder::Ascending => query.push(" ORDER BY j.created_at ASC, j.id ASC LIMIT "),
        MediaJobOrder::Descending => query.push(" ORDER BY j.created_at DESC, j.id DESC LIMIT "),
    };
    query.push_bind(i64::from(limit) + 1);
    let rows = query
        .build_query_as::<MediaJobRow>()
        .fetch_all(pool)
        .await?;
    let items = rows
        .into_iter()
        .map(media_job_from_row)
        .collect::<Result<Vec<_>, _>>()?;
    let (items, next_cursor) = split_page(items, usize::from(limit), |last| last.id.to_string());
    Ok(Page { items, next_cursor })
}

fn push_filters(query: &mut QueryBuilder<Postgres>, filters: &MediaJobFilters) {
    if let Some(value) = filters.api_key_id {
        query.push(" AND j.api_key_id = ").push_bind(value);
    }
    if let Some(value) = filters.provider_id {
        query.push(" AND j.provider_id = ").push_bind(value);
    }
    if let Some(value) = &filters.route_slug {
        query.push(" AND j.route_slug = ").push_bind(value);
    }
    if !filters.route_slugs.is_empty() {
        query
            .push(" AND j.route_slug = ANY(")
            .push_bind(&filters.route_slugs)
            .push("::text[])");
    }
    if let Some(value) = filters.operation {
        query.push(" AND j.operation = ").push_bind(value.as_str());
    }
    if let Some(value) = filters.surface {
        query.push(" AND j.surface = ").push_bind(value.as_str());
    }
    if let Some(value) = filters.state {
        query
            .push(" AND j.state = ")
            .push_bind(value.as_str())
            .push("::media_job_state");
    }
    if let Some(value) = filters.lifecycle {
        query
            .push(" AND j.lifecycle_state = ")
            .push_bind(value.as_str());
    }
    if let Some(value) = filters.created_after {
        query.push(" AND j.created_at >= ").push_bind(value);
    }
    if let Some(value) = filters.created_before {
        query.push(" AND j.created_at < ").push_bind(value);
    }
}

#[derive(Debug, FromRow)]
pub(crate) struct MediaJobRow {
    pub(crate) id: Uuid,
    pub(crate) upstream_job_id: Option<String>,
    pub(crate) api_key_id: Uuid,
    pub(crate) provider_id: Uuid,
    pub(crate) provider_name: String,
    pub(crate) provider_model: String,
    pub(crate) route_slug: String,
    pub(crate) operation: String,
    pub(crate) surface: String,
    pub(crate) state: String,
    pub(crate) lifecycle_state: String,
    pub(crate) progress_percent: Option<f32>,
    pub(crate) content_available: bool,
    pub(crate) expires_at: Option<DateTime<Utc>>,
    pub(crate) error_class: Option<String>,
    pub(crate) completed_at: Option<DateTime<Utc>>,
    pub(crate) last_polled_at: Option<DateTime<Utc>>,
    pub(crate) reconciliation_error: Option<String>,
    pub(crate) deleted_at: Option<DateTime<Utc>>,
    pub(crate) runtime_generation_id: Uuid,
    pub(crate) provider_revision_id: Uuid,
    pub(crate) reconciliation_claim_id: Option<Uuid>,
    pub(crate) reconciliation_attempts: i32,
    pub(crate) next_reconciliation_at: DateTime<Utc>,
    pub(crate) last_reconciliation_at: Option<DateTime<Utc>>,
    pub(crate) etag: Uuid,
    pub(crate) created_at: DateTime<Utc>,
    pub(crate) updated_at: DateTime<Utc>,
}

pub(crate) fn media_job_from_row(row: MediaJobRow) -> Result<MediaJobRecord, MediaJobError> {
    Ok(MediaJobRecord {
        id: row.id,
        upstream_job_id: row.upstream_job_id,
        api_key_id: row.api_key_id,
        provider_id: row.provider_id,
        provider_name: row.provider_name,
        upstream_model: row.provider_model,
        route_slug: row.route_slug,
        operation: row.operation.parse().map_err(|_| {
            MediaJobError::Invalid("database returned an unknown operation".to_owned())
        })?,
        surface: row.surface.parse().map_err(|_| {
            MediaJobError::Invalid("database returned an unknown surface".to_owned())
        })?,
        state: MediaJobState::parse(&row.state)?,
        lifecycle: MediaJobLifecycle::parse(&row.lifecycle_state)?,
        progress_percent: row.progress_percent,
        content_available: row.content_available,
        expires_at: row.expires_at,
        error_class: row.error_class,
        completed_at: row.completed_at,
        last_polled_at: row.last_polled_at,
        reconciliation_error: row.reconciliation_error,
        deleted_at: row.deleted_at,
        runtime_generation_id: row.runtime_generation_id,
        provider_revision_id: row.provider_revision_id,
        reconciliation_claim_id: row.reconciliation_claim_id,
        reconciliation_attempts: u32::try_from(row.reconciliation_attempts).map_err(|_| {
            MediaJobError::Invalid("reconciliation attempt count is invalid".to_owned())
        })?,
        next_reconciliation_at: row.next_reconciliation_at,
        last_reconciliation_at: row.last_reconciliation_at,
        etag: row.etag,
        created_at: row.created_at,
        updated_at: row.updated_at,
    })
}

#[derive(sqlx::FromRow)]
struct MediaJobsAfterIdRow {
    created_at: chrono::DateTime<chrono::Utc>,
    id: uuid::Uuid,
}
