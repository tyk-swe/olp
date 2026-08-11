use std::collections::BTreeMap;

use serde::{Serialize, de::DeserializeOwned};
use serde_json::Value;

pub(in crate::protocols) enum PointerExtensionError {
    InvalidPath(String),
    Json(serde_json::Error),
}

pub(in crate::protocols) fn collect_extra(
    prefix: &str,
    extra: &BTreeMap<String, Value>,
    extensions: &mut BTreeMap<String, Value>,
) {
    for (key, value) in extra {
        extensions.insert(
            format!("{prefix}/{}", escape_json_pointer(key)),
            value.clone(),
        );
    }
}

pub(in crate::protocols) fn escape_json_pointer(value: &str) -> String {
    value.replace('~', "~0").replace('/', "~1")
}

pub(in crate::protocols) fn unescape_json_pointer(value: &str) -> String {
    value.replace("~1", "/").replace("~0", "~")
}

/// Restores extensions located directly on a wire object. Nested extensions
/// are handled by the operation-specific codec so malformed paths fail closed.
pub(in crate::protocols) fn apply_flat_extensions(
    extra: &mut BTreeMap<String, Value>,
    extensions: &BTreeMap<String, Value>,
) -> Result<(), String> {
    for (pointer, value) in extensions {
        let Some(field) = pointer.strip_prefix('/') else {
            return Err(pointer.clone());
        };
        if field.is_empty() || field.contains('/') {
            return Err(pointer.clone());
        }
        extra.insert(unescape_json_pointer(field), value.clone());
    }
    Ok(())
}

/// Applies captured JSON-pointer fields back to the same wire protocol without
/// allowing an extension to overwrite a canonical field.
pub(in crate::protocols) fn apply_pointer_extensions<T>(
    wire: T,
    extensions: &BTreeMap<String, Value>,
) -> Result<T, String>
where
    T: Serialize + DeserializeOwned,
{
    let mut value = serde_json::to_value(wire).map_err(|error| error.to_string())?;
    for (pointer, extension) in extensions {
        let segments = pointer
            .strip_prefix('/')
            .filter(|value| !value.is_empty())
            .ok_or_else(|| pointer.clone())?
            .split('/')
            .map(unescape_json_pointer)
            .collect::<Vec<_>>();
        let (last, parents) = segments.split_last().ok_or_else(|| pointer.clone())?;
        let mut cursor = &mut value;
        for segment in parents {
            cursor = match cursor {
                Value::Object(object) => object.get_mut(segment),
                Value::Array(array) => segment
                    .parse::<usize>()
                    .ok()
                    .and_then(|index| array.get_mut(index)),
                _ => None,
            }
            .ok_or_else(|| pointer.clone())?;
        }
        let Value::Object(object) = cursor else {
            return Err(pointer.clone());
        };
        if object.contains_key(last) {
            return Err(pointer.clone());
        }
        object.insert(last.clone(), extension.clone());
    }
    serde_json::from_value(value).map_err(|_| "extension made the wire object invalid".into())
}

pub(in crate::protocols) fn apply_request_extensions<T>(
    request: &mut T,
    extensions: &BTreeMap<String, Value>,
) -> Result<(), PointerExtensionError>
where
    T: Serialize + DeserializeOwned,
{
    if extensions.is_empty() {
        return Ok(());
    }
    let mut value = serde_json::to_value(&*request).map_err(PointerExtensionError::Json)?;
    let mut insertions = extensions
        .iter()
        .filter(|(path, _)| is_array_item_path(path))
        .collect::<Vec<_>>();
    insertions.sort_by_key(|(path, _)| array_path_key(path));
    for (path, extension) in insertions {
        set_request_pointer(&mut value, path, extension.clone(), true)?;
    }
    for (path, extension) in extensions {
        if !is_array_item_path(path) {
            set_request_pointer(&mut value, path, extension.clone(), false)?;
        }
    }
    *request = serde_json::from_value(value).map_err(PointerExtensionError::Json)?;
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

fn set_request_pointer(
    root: &mut Value,
    pointer: &str,
    value: Value,
    insert_array_item: bool,
) -> Result<(), PointerExtensionError> {
    let invalid_path = || PointerExtensionError::InvalidPath(pointer.to_owned());
    let segments = pointer
        .strip_prefix('/')
        .ok_or_else(invalid_path)?
        .split('/')
        .map(unescape_json_pointer)
        .collect::<Vec<_>>();
    if segments.is_empty() || segments.len() > 16 {
        return Err(invalid_path());
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
                let index = segment.parse::<usize>().map_err(|_| invalid_path())?;
                if insert_array_item && index <= array.len() {
                    array.insert(index, value);
                    return Ok(());
                }
                *array.get_mut(index).ok_or_else(invalid_path)? = value;
                return Ok(());
            }
            Value::Object(object) => {
                current = object.get_mut(segment).ok_or_else(invalid_path)?;
            }
            Value::Array(array) => {
                let index = segment.parse::<usize>().map_err(|_| invalid_path())?;
                current = array.get_mut(index).ok_or_else(invalid_path)?;
            }
            _ => return Err(invalid_path()),
        }
    }
    Err(invalid_path())
}

pub(in crate::protocols) fn apply_response_extensions<T>(
    response: T,
    extensions: &BTreeMap<String, Value>,
) -> Result<T, PointerExtensionError>
where
    T: Serialize + DeserializeOwned,
{
    let mut value = serde_json::to_value(response).map_err(PointerExtensionError::Json)?;
    let mut entries = extensions.iter().collect::<Vec<_>>();
    entries.sort_by_key(|(pointer, _)| pointer_depth(pointer));
    for (pointer, extension) in entries {
        insert_response_pointer(&mut value, pointer, extension.clone())?;
    }
    serde_json::from_value(value).map_err(PointerExtensionError::Json)
}

fn insert_response_pointer(
    root: &mut Value,
    pointer: &str,
    value: Value,
) -> Result<(), PointerExtensionError> {
    let invalid_path = || PointerExtensionError::InvalidPath(pointer.to_owned());
    let segments = pointer
        .strip_prefix('/')
        .filter(|pointer| !pointer.is_empty())
        .ok_or_else(invalid_path)?
        .split('/')
        .map(unescape_json_pointer)
        .collect::<Vec<_>>();
    if segments.len() > 24 {
        return Err(invalid_path());
    }
    let (terminal, parents) = segments.split_last().ok_or_else(invalid_path)?;
    let mut current = root;
    for segment in parents {
        current = match current {
            Value::Object(object) => object.get_mut(segment),
            Value::Array(array) => segment
                .parse::<usize>()
                .ok()
                .and_then(|index| array.get_mut(index)),
            _ => None,
        }
        .ok_or_else(invalid_path)?;
    }
    match current {
        Value::Object(object) if !object.contains_key(terminal) => {
            object.insert(terminal.clone(), value);
            Ok(())
        }
        Value::Array(array) => {
            let index = terminal.parse::<usize>().map_err(|_| invalid_path())?;
            if index <= array.len() {
                array.insert(index, value);
                Ok(())
            } else {
                Err(invalid_path())
            }
        }
        _ => Err(invalid_path()),
    }
}

fn pointer_depth(pointer: &str) -> usize {
    pointer.bytes().filter(|byte| *byte == b'/').count()
}

pub(in crate::protocols) fn insert_flat_extension(
    root: &mut Value,
    pointer: &str,
    value: Value,
) -> Result<(), String> {
    let key = pointer
        .strip_prefix('/')
        .filter(|key| !key.is_empty() && !key.contains('/'))
        .map(unescape_json_pointer)
        .ok_or_else(|| pointer.to_owned())?;
    let object = root.as_object_mut().ok_or_else(|| pointer.to_owned())?;
    if object.contains_key(&key) {
        return Err(pointer.to_owned());
    }
    object.insert(key, value);
    Ok(())
}
