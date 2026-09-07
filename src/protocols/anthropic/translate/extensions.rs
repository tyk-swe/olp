use std::collections::BTreeMap;

use serde_json::Value;

use crate::protocols::extensions::PointerExtensionError;
use crate::protocols::extensions::apply_request_extensions;

use crate::protocols::anthropic::dto::MessagesRequest;
use crate::protocols::anthropic::translate::errors::DecodeError;
use crate::protocols::anthropic::translate::errors::EncodeError;
use crate::protocols::anthropic::translate::errors::ResponseError;

pub(crate) fn require_kind(actual: &str, expected: &'static str) -> Result<(), DecodeError> {
    if actual == expected {
        Ok(())
    } else {
        Err(DecodeError::UnexpectedType {
            expected,
            actual: actual.to_owned(),
        })
    }
}

pub(crate) fn require_response_kind(
    actual: &str,
    expected: &'static str,
) -> Result<(), ResponseError> {
    if actual == expected {
        Ok(())
    } else {
        Err(ResponseError::UnexpectedType(actual.to_owned()))
    }
}

pub(crate) fn apply_extensions(
    request: &mut MessagesRequest,
    extensions: &BTreeMap<String, Value>,
) -> Result<(), EncodeError> {
    apply_request_extensions(request, extensions).map_err(|error| match error {
        PointerExtensionError::InvalidPath(path) => EncodeError::InvalidExtensionPath(path),
        PointerExtensionError::Json(error) => EncodeError::Json(error),
    })
}
