use std::collections::BTreeMap;

use serde_json::Value;

use super::super::dto::MessagesRequest;
use super::errors::{DecodeError, EncodeError, ResponseError};
use crate::json_pointer::ApplyExtensionsError;

pub(super) fn require_kind(actual: &str, expected: &'static str) -> Result<(), DecodeError> {
    if actual == expected {
        Ok(())
    } else {
        Err(DecodeError::UnexpectedType {
            expected,
            actual: actual.to_owned(),
        })
    }
}

pub(super) fn require_response_kind(
    actual: &str,
    expected: &'static str,
) -> Result<(), ResponseError> {
    if actual == expected {
        Ok(())
    } else {
        Err(ResponseError::UnexpectedType(actual.to_owned()))
    }
}

pub(crate) fn collect_extra(
    prefix: &str,
    extra: &BTreeMap<String, Value>,
    extensions: &mut BTreeMap<String, Value>,
) {
    crate::json_pointer::collect_extra(prefix, extra, extensions);
}

pub(super) fn apply_extensions(
    request: &mut MessagesRequest,
    extensions: &BTreeMap<String, Value>,
) -> Result<(), EncodeError> {
    crate::json_pointer::apply_extensions(request, extensions, &["tools"]).map_err(|error| {
        match error {
            ApplyExtensionsError::Json(error) => EncodeError::Json(error),
            ApplyExtensionsError::InvalidPath(path) => EncodeError::InvalidExtensionPath(path),
        }
    })
}
