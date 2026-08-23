use axum::http::StatusCode;
use chrono::{DateTime, Utc};
use olp_db::operations::cursor::{Error, Timestamp};
use serde::Deserialize;
use tracing::error;
use utoipa::IntoParams;

use crate::{
    management::error_mapping::{
        etag_mismatch, idempotency_in_progress, idempotency_key_reused, map_persistence,
    },
    public_http::problem::Problem,
};

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub(super) struct PageQuery {
    pub(super) cursor: Option<String>,
    pub(super) limit: Option<u16>,
}

pub(super) fn page_limit(value: Option<u16>) -> Result<u16, Problem> {
    let value = value.unwrap_or(50);
    if (1..=200).contains(&value) {
        return Ok(value);
    }
    Err(Problem::field_validation(
        "limit",
        "Use a page size between 1 and 200.",
    ))
}

pub(super) fn timestamp_cursor(value: Option<&str>) -> Result<Option<Timestamp>, Problem> {
    value
        .map(Timestamp::parse)
        .transpose()
        .map_err(map_operations)
}

pub(super) fn validate_time_range(
    start_name: &str,
    start: DateTime<Utc>,
    end_name: &str,
    end: DateTime<Utc>,
) -> Result<(), Problem> {
    if start < end {
        return Ok(());
    }
    Err(Problem::field_validation(
        end_name,
        format!("{end_name} must be later than {start_name}."),
    ))
}

pub(super) fn not_found() -> Problem {
    Problem::new(
        StatusCode::NOT_FOUND,
        "resource_not_found",
        "Resource not found",
        "The requested resource does not exist.",
    )
}

pub(super) fn map_operations(error: Error) -> Problem {
    match error {
        Error::InvalidCursor => {
            Problem::bad_request("invalid_cursor", "The cursor is invalid or expired.")
        }
        Error::NotFound => not_found(),
        Error::PreconditionFailed => etag_mismatch("resource"),
        Error::IdempotencyConflict => idempotency_key_reused(),
        Error::IdempotencyInProgress => idempotency_in_progress(),
        Error::Invalid(message) => Problem::field_validation("request", message),
        Error::Database(error) => {
            error!(%error, "operations persistence query failed");
            Problem::internal()
        }
        Error::Persistence(error) => map_persistence(error),
    }
}
