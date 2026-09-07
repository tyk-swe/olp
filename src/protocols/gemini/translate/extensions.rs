use std::collections::BTreeMap;

use serde_json::Value;

use crate::protocols::extensions::PointerExtensionError;
use crate::protocols::extensions::apply_request_extensions;

use crate::protocols::gemini::dto::GenerateContentRequest;
use crate::protocols::gemini::translate::errors::EncodeError;

pub(crate) fn apply_extensions(
    request: &mut GenerateContentRequest,
    extensions: &BTreeMap<String, Value>,
) -> Result<(), EncodeError> {
    apply_request_extensions(request, extensions).map_err(|error| match error {
        PointerExtensionError::InvalidPath(path) => EncodeError::InvalidExtensionPath(path),
        PointerExtensionError::Json(error) => EncodeError::Json(error),
    })
}
