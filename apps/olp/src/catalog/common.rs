use axum::{
    Json,
    extract::rejection::JsonRejection,
    http::{HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
};
use olp_storage::CatalogError;
use serde::{Deserialize, Serialize};
use tracing::error;
use utoipa::ToSchema;
use uuid::Uuid;

use crate::{FieldErrors, Problem};

#[derive(Debug, Deserialize, ToSchema)]
pub(super) struct PageQuery {
    pub cursor: Option<String>,
    pub limit: Option<u16>,
}

#[derive(Debug, Deserialize)]
pub(super) struct DiffQuery {
    pub from: Uuid,
    pub to: Uuid,
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct RuntimeGenerationCatalogResponse {
    pub id: Uuid,
    pub sequence: i64,
}

pub(super) fn page(query: PageQuery) -> Result<(Option<Uuid>, i64), Problem> {
    let cursor = query
        .cursor
        .map(|value| {
            Uuid::parse_str(&value).map_err(|_| {
                Problem::bad_request("invalid_cursor", "The pagination cursor is invalid.")
            })
        })
        .transpose()?;
    let limit = query.limit.unwrap_or(50);
    if !(1..=100).contains(&limit) {
        return Err(Problem::bad_request(
            "invalid_page_size",
            "Page size must be between 1 and 100.",
        ));
    }
    Ok((cursor, i64::from(limit)))
}

pub(super) fn with_etag(response: impl IntoResponse, etag: Uuid) -> Result<Response, Problem> {
    let mut response = response.into_response();
    response.headers_mut().insert(
        header::ETAG,
        HeaderValue::from_str(&format!("\"{etag}\"")).map_err(|_| Problem::internal())?,
    );
    Ok(response)
}

pub(super) fn json<T>(payload: Result<Json<T>, JsonRejection>) -> Result<T, Problem> {
    payload.map(|Json(value)| value).map_err(|error| {
        Problem::bad_request("invalid_json", format!("Request body is invalid: {error}"))
    })
}

pub(super) fn validation(field: &str, detail: &str) -> Problem {
    let mut errors = FieldErrors::new();
    errors.insert(field.to_owned(), vec![detail.to_owned()]);
    Problem::validation(errors)
}

pub(super) fn map_catalog(error: CatalogError) -> Problem {
    match error {
        CatalogError::NotFound => Problem::new(
            StatusCode::NOT_FOUND,
            "catalog_resource_not_found",
            "Resource not found",
            "The requested catalog resource does not exist.",
        ),
        CatalogError::PreconditionFailed => Problem::new(
            StatusCode::PRECONDITION_FAILED,
            "etag_mismatch",
            "Precondition failed",
            "The resource changed after it was loaded. Refresh and retry.",
        ),
        CatalogError::InUse => Problem::conflict(
            "catalog_resource_in_use",
            "The resource is active or referenced and cannot be removed.",
        ),
        CatalogError::Invalid(detail) => validation("catalog", &detail),
        CatalogError::ProviderRevisionDiffTooLarge { dimension, maximum } => validation(
            "revisions",
            &format!("provider revision diff supports at most {maximum} {dimension} per revision"),
        ),
        CatalogError::IdempotencyConflict => Problem::conflict(
            "idempotency_key_reused",
            "This Idempotency-Key has already been used for this operation.",
        ),
        CatalogError::IdempotencyInProgress => Problem::conflict(
            "idempotency_in_progress",
            "An operation with this Idempotency-Key is still in progress.",
        ),
        CatalogError::Persistence(error) => crate::management::map_persistence(error),
        CatalogError::RuntimeCompile(error) => {
            error!(%error, "runtime compilation failed after catalog mutation");
            Problem::internal()
        }
    }
}
