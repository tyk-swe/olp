use axum::{
    extract::{Query, rejection::QueryRejection},
    http::StatusCode,
};
use chrono::{DateTime, Utc};
use olp_db::operations::cursor::{Error, Timestamp};
use serde::Deserialize;
use tracing::error;
use utoipa::IntoParams;

use crate::{management::error_mapping::map_persistence, public_http::problem::Problem};

// One page-size contract for every management collection.
pub(super) use crate::management::pagination::page_limit;

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub(super) struct PageQuery {
    /// Opaque cursor returned by the previous page.
    pub(super) cursor: Option<String>,
    /// Page size, from 1 to 200. Defaults to 50.
    #[param(minimum = 1, maximum = 200)]
    pub(super) limit: Option<u16>,
}

/// Turns Axum's typed-query rejection into a problem+json response so a
/// malformed UUID, timestamp, or page size never escapes as a bare text/plain
/// 400. The rejection's own message can quote the offending value, so the
/// detail deliberately describes only the failure, never the input.
pub(super) fn query_parameters<T>(query: Result<Query<T>, QueryRejection>) -> Result<T, Problem> {
    query.map(|Query(value)| value).map_err(|_| {
        Problem::bad_request(
            "invalid_query_parameters",
            "One or more query parameters are missing or malformed.",
        )
    })
}

/// Treats a blank or whitespace-only filter as absent. Console inputs submit an
/// empty string when a filter is cleared, and an exact-match filter on "" would
/// otherwise return nothing at all.
pub(super) fn optional_filter(value: Option<&String>) -> Option<String> {
    value
        .map(|value| value.trim())
        .filter(|value| !value.is_empty())
        .map(ToOwned::to_owned)
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
        Error::PreconditionFailed => Problem::new(
            StatusCode::PRECONDITION_FAILED,
            "etag_mismatch",
            "Precondition failed",
            "The resource changed; refresh it and retry with the current ETag.",
        ),
        Error::IdempotencyConflict => Problem::conflict(
            "idempotency_key_reused",
            "The Idempotency-Key has already been used for this operation.",
        ),
        Error::IdempotencyInProgress => Problem::conflict(
            "idempotency_in_progress",
            "An operation with this Idempotency-Key is still in progress.",
        ),
        Error::Invalid(message) => Problem::field_validation("request", message),
        Error::Database(error) => {
            error!(%error, "operations persistence query failed");
            Problem::internal()
        }
        Error::Persistence(error) => map_persistence(error),
    }
}
