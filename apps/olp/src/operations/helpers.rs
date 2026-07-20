use axum::http::StatusCode;
use chrono::{DateTime, Utc};
use olp_storage::OperationsError;
use serde::Deserialize;
use tracing::error;
use utoipa::IntoParams;

use crate::{FieldErrors, Problem, management_api::map_persistence};

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
    let mut errors = FieldErrors::new();
    errors.insert(
        "limit".to_owned(),
        vec!["Use a page size between 1 and 200.".to_owned()],
    );
    Err(Problem::validation(errors))
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
    let mut errors = FieldErrors::new();
    errors.insert(
        end_name.to_owned(),
        vec![format!("{end_name} must be later than {start_name}.")],
    );
    Err(Problem::validation(errors))
}

pub(super) fn not_found() -> Problem {
    Problem::new(
        StatusCode::NOT_FOUND,
        "resource_not_found",
        "Resource not found",
        "The requested resource does not exist.",
    )
}

pub(super) fn map_operations(error: OperationsError) -> Problem {
    match error {
        OperationsError::InvalidCursor => {
            Problem::bad_request("invalid_cursor", "The cursor is invalid or expired.")
        }
        OperationsError::NotFound => not_found(),
        OperationsError::PreconditionFailed => Problem::new(
            StatusCode::PRECONDITION_FAILED,
            "etag_mismatch",
            "Precondition failed",
            "The resource changed; refresh it and retry with the current ETag.",
        ),
        OperationsError::IdempotencyConflict => Problem::conflict(
            "idempotency_key_reused",
            "The Idempotency-Key has already been used for this operation.",
        ),
        OperationsError::IdempotencyInProgress => Problem::conflict(
            "idempotency_in_progress",
            "An operation with this Idempotency-Key is still in progress.",
        ),
        OperationsError::Invalid(message) => {
            let mut errors = FieldErrors::new();
            errors.insert("request".to_owned(), vec![message]);
            Problem::validation(errors)
        }
        OperationsError::Database(error) => {
            error!(%error, "operations persistence query failed");
            Problem::internal()
        }
        OperationsError::Persistence(error) => map_persistence(error),
    }
}
