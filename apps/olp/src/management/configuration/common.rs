use axum::{Json, extract::rejection::JsonRejection};
use olp_storage::configuration::ConfigurationError;
use serde::Deserialize;
use uuid::Uuid;

use crate::{FieldErrors, Problem};

pub(crate) use crate::management::{PageQuery, page, with_etag};

#[derive(Debug, Deserialize)]
pub(crate) struct DiffQuery {
    pub from: Uuid,
    pub to: Uuid,
}

pub(super) fn json<T>(payload: Result<Json<T>, JsonRejection>) -> Result<T, Problem> {
    payload.map(|Json(value)| value).map_err(|error| {
        Problem::bad_request("invalid_json", format!("Request body is invalid: {error}"))
    })
}

pub(crate) fn validation(field: &str, detail: &str) -> Problem {
    let mut errors = FieldErrors::new();
    errors.insert(field.to_owned(), vec![detail.to_owned()]);
    Problem::validation(errors)
}

pub(crate) fn map_configuration_resource(error: ConfigurationError) -> Problem {
    crate::management::error_mapping::map_configuration(error)
}
