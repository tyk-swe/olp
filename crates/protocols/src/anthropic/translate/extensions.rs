use std::collections::BTreeMap;

use serde_json::Value;

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

pub(crate) fn collect_extra(
    prefix: &str,
    extra: &BTreeMap<String, Value>,
    extensions: &mut BTreeMap<String, Value>,
) {
    for (key, value) in extra {
        extensions.insert(format!("{prefix}/{}", escape_pointer(key)), value.clone());
    }
}

fn escape_pointer(value: &str) -> String {
    value.replace('~', "~0").replace('/', "~1")
}

fn unescape_pointer(value: &str) -> String {
    value.replace("~1", "/").replace("~0", "~")
}

pub(super) fn apply_extensions(
    request: &mut MessagesRequest,
    extensions: &BTreeMap<String, Value>,
) -> Result<(), EncodeError> {
    if extensions.is_empty() {
        return Ok(());
    }
    let mut value = serde_json::to_value(&*request).map_err(EncodeError::Json)?;
    let mut insertions = extensions
        .iter()
        .filter(|(path, _)| is_array_item_path(path))
        .collect::<Vec<_>>();
    insertions.sort_by_key(|(path, _)| array_path_key(path));
    for (path, extension) in insertions {
        set_pointer(&mut value, path, extension.clone(), true)?;
    }
    for (path, extension) in extensions {
        if !is_array_item_path(path) {
            set_pointer(&mut value, path, extension.clone(), false)?;
        }
    }
    *request = serde_json::from_value(value).map_err(EncodeError::Json)?;
    Ok(())
}

fn is_array_item_path(path: &str) -> bool {
    let segments = path.trim_start_matches('/').split('/').collect::<Vec<_>>();
    matches!(segments.as_slice(), ["tools", index] if index.parse::<usize>().is_ok())
}

fn array_path_key(path: &str) -> (String, usize) {
    let (parent, index) = path.rsplit_once('/').unwrap_or((path, "0"));
    (parent.to_owned(), index.parse().unwrap_or(0))
}

fn set_pointer(
    root: &mut Value,
    pointer: &str,
    value: Value,
    insert_array_item: bool,
) -> Result<(), EncodeError> {
    let segments = pointer
        .strip_prefix('/')
        .ok_or_else(|| EncodeError::InvalidExtensionPath(pointer.to_owned()))?
        .split('/')
        .map(unescape_pointer)
        .collect::<Vec<_>>();
    if segments.is_empty() || segments.len() > 16 {
        return Err(EncodeError::InvalidExtensionPath(pointer.to_owned()));
    }
    let mut current = root;
    for (position, segment) in segments.iter().enumerate() {
        let terminal = position + 1 == segments.len();
        match current {
            Value::Object(object) if terminal => {
                object.insert(segment.clone(), value);
                return Ok(());
            }
            Value::Array(array) if terminal => {
                let index = segment
                    .parse::<usize>()
                    .map_err(|_| EncodeError::InvalidExtensionPath(pointer.to_owned()))?;
                if insert_array_item && index <= array.len() {
                    array.insert(index, value);
                    return Ok(());
                }
                let slot = array
                    .get_mut(index)
                    .ok_or_else(|| EncodeError::InvalidExtensionPath(pointer.to_owned()))?;
                *slot = value;
                return Ok(());
            }
            Value::Object(object) => {
                current = object
                    .get_mut(segment)
                    .ok_or_else(|| EncodeError::InvalidExtensionPath(pointer.to_owned()))?;
            }
            Value::Array(array) => {
                let index = segment
                    .parse::<usize>()
                    .map_err(|_| EncodeError::InvalidExtensionPath(pointer.to_owned()))?;
                current = array
                    .get_mut(index)
                    .ok_or_else(|| EncodeError::InvalidExtensionPath(pointer.to_owned()))?;
            }
            _ => return Err(EncodeError::InvalidExtensionPath(pointer.to_owned())),
        }
    }
    Err(EncodeError::InvalidExtensionPath(pointer.to_owned()))
}
