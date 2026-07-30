use std::collections::BTreeMap;

use serde_json::Value;

pub(crate) use crate::extensions::collect_extra;
use crate::extensions::{PointerExtensionError, apply_request_extensions};

use super::super::dto::GenerateContentRequest;
use super::errors::EncodeError;

pub(super) fn apply_extensions(
    request: &mut GenerateContentRequest,
    extensions: &BTreeMap<String, Value>,
) -> Result<(), EncodeError> {
    apply_request_extensions(request, extensions).map_err(|error| match error {
        PointerExtensionError::InvalidPath(path) => EncodeError::InvalidExtensionPath(path),
        PointerExtensionError::Json(error) => EncodeError::Json(error),
    })
}
