use std::collections::BTreeMap;

use serde_json::Value;

use super::super::dto::GenerateContentRequest;
use super::errors::EncodeError;
use crate::json_pointer::ApplyExtensionsError;

pub(crate) fn collect_extra(
    prefix: &str,
    extra: &BTreeMap<String, Value>,
    extensions: &mut BTreeMap<String, Value>,
) {
    crate::json_pointer::collect_extra(prefix, extra, extensions);
}

pub(super) fn apply_extensions(
    request: &mut GenerateContentRequest,
    extensions: &BTreeMap<String, Value>,
) -> Result<(), EncodeError> {
    crate::json_pointer::apply_extensions(request, extensions, &["tools"]).map_err(|error| {
        match error {
            ApplyExtensionsError::Json(error) => EncodeError::Json(error),
            ApplyExtensionsError::InvalidPath(path) => EncodeError::InvalidExtensionPath(path),
        }
    })
}
