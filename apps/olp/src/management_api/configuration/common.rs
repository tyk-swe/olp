use serde::Deserialize;
use uuid::Uuid;

use crate::{FieldErrors, Problem};

pub(crate) use crate::management_api::common::map_configuration as map_configuration_resource;
pub(crate) use crate::management_api::{PageQuery, json_payload as json, page, with_etag};

#[derive(Debug, Deserialize)]
pub(crate) struct DiffQuery {
    pub from: Uuid,
    pub to: Uuid,
}

pub(crate) fn validation(field: &str, detail: &str) -> Problem {
    let mut errors = FieldErrors::new();
    errors.insert(field.to_owned(), vec![detail.to_owned()]);
    Problem::validation(errors)
}
