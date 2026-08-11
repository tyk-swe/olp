use std::collections::BTreeMap;

use serde_json::Value;

pub(in crate::protocols) use crate::protocols::extensions::collect_extra;
use crate::protocols::extensions::{PointerExtensionError, apply_request_extensions};

use super::super::dto::MessagesRequest;
use super::errors::{DecodeError, EncodeError, ResponseError};

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

pub(super) fn apply_extensions(
    request: &mut MessagesRequest,
    extensions: &BTreeMap<String, Value>,
) -> Result<(), EncodeError> {
    apply_request_extensions(request, extensions).map_err(|error| match error {
        PointerExtensionError::InvalidPath(path) => EncodeError::InvalidExtensionPath(path),
        PointerExtensionError::Json(error) => EncodeError::Json(error),
    })
}
