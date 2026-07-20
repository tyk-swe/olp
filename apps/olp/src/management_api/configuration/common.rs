use axum::{
    Json,
    extract::rejection::JsonRejection,
    http::{HeaderValue, header},
    response::{IntoResponse, Response},
};
use olp_storage::ConfigurationError;
use serde::Deserialize;
use utoipa::ToSchema;
use uuid::Uuid;

use crate::{FieldErrors, Problem};

#[derive(Debug, Deserialize, ToSchema)]
pub(crate) struct PageQuery {
    pub cursor: Option<String>,
    pub limit: Option<u16>,
}

#[derive(Debug, Deserialize)]
pub(crate) struct DiffQuery {
    pub from: Uuid,
    pub to: Uuid,
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

pub(crate) fn map_configuration_resource(error: ConfigurationError) -> Problem {
    crate::management_api::common::map_configuration(error)
}
