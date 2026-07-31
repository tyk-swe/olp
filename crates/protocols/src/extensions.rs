use std::cmp::Ordering;
use std::collections::BTreeMap;

use serde::{Serialize, de::DeserializeOwned};
use serde_json::Value;

pub(crate) enum PointerExtensionError {
    InvalidPath(String),
    Json(serde_json::Error),
}

pub(crate) fn collect_extra(
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

pub(crate) fn escape_json_pointer(value: &str) -> String {
    value.replace('~', "~0").replace('/', "~1")
}

pub(crate) fn unescape_json_pointer(value: &str) -> Option<String> {
    let mut decoded = String::with_capacity(value.len());
    let mut chars = value.chars();
    while let Some(character) = chars.next() {
        if character != '~' {
            decoded.push(character);
            continue;
        }
        decoded.push(match chars.next()? {
            '0' => '~',
            '1' => '/',
            _ => return None,
        });
    }
    Some(decoded)
}

/// Restores extensions located directly on a wire object. Nested extensions
/// are handled by the operation-specific codec so malformed paths fail closed.
pub(crate) fn apply_flat_extensions(
    extra: &mut BTreeMap<String, Value>,
    extensions: &BTreeMap<String, Value>,
) -> Result<(), String> {
    for (pointer, value) in extensions {
        let Some(field) = pointer.strip_prefix('/') else {
            return Err(pointer.clone());
        };
        if field.contains('/') {
            return Err(pointer.clone());
        }
        extra.insert(
            unescape_json_pointer(field).ok_or_else(|| pointer.clone())?,
            value.clone(),
        );
    }
    Ok(())
}

/// Applies captured JSON-pointer fields back to the same wire protocol without
/// allowing an extension to overwrite a canonical field.
pub(crate) fn apply_pointer_extensions<T>(
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
            .ok_or_else(|| pointer.clone())?
            .split('/')
            .map(unescape_json_pointer)
            .collect::<Option<Vec<_>>>()
            .ok_or_else(|| pointer.clone())?;
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

pub(crate) fn apply_request_extensions<T>(
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
        .collect::<Option<Vec<_>>>()
        .ok_or_else(invalid_path)?;
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

pub(crate) fn apply_response_extensions<T>(
    response: T,
    extensions: &BTreeMap<String, Value>,
) -> Result<T, PointerExtensionError>
where
    T: Serialize + DeserializeOwned,
{
    let mut value = serde_json::to_value(response).map_err(PointerExtensionError::Json)?;
    let mut entries = extensions.iter().collect::<Vec<_>>();
    entries.sort_by(|(left, _), (right, _)| response_pointer_order(left, right));
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
        .ok_or_else(invalid_path)?
        .split('/')
        .map(unescape_json_pointer)
        .collect::<Option<Vec<_>>>()
        .ok_or_else(invalid_path)?;
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

fn response_pointer_order(left: &str, right: &str) -> Ordering {
    pointer_depth(left)
        .cmp(&pointer_depth(right))
        .then_with(|| {
            let (left_parent, left_terminal) = left.rsplit_once('/').unwrap_or((left, ""));
            let (right_parent, right_terminal) = right.rsplit_once('/').unwrap_or((right, ""));
            left_parent.cmp(right_parent).then_with(|| {
                match (
                    left_terminal.parse::<usize>(),
                    right_terminal.parse::<usize>(),
                ) {
                    (Ok(left), Ok(right)) => left.cmp(&right),
                    _ => left_terminal.cmp(right_terminal),
                }
            })
        })
}

pub(crate) fn insert_flat_extension(
    root: &mut Value,
    pointer: &str,
    value: Value,
) -> Result<(), String> {
    let key = pointer
        .strip_prefix('/')
        .filter(|key| !key.contains('/'))
        .and_then(unescape_json_pointer)
        .ok_or_else(|| pointer.to_owned())?;
    let object = root.as_object_mut().ok_or_else(|| pointer.to_owned())?;
    if object.contains_key(&key) {
        return Err(pointer.to_owned());
    }
    object.insert(key, value);
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn response_array_items_are_restored_in_numeric_index_order() {
        let response = serde_json::json!({
            "items": [0, 1, 3, 4, 5, 6, 7, 8, 9, 11]
        });
        let extensions = BTreeMap::from([
            ("/".to_owned(), Value::from("empty key")),
            ("/items/2".to_owned(), Value::from(2)),
            ("/items/10".to_owned(), Value::from(10)),
        ]);

        let Ok(restored): Result<Value, _> = apply_response_extensions(response, &extensions)
        else {
            panic!("response extensions must apply");
        };

        assert_eq!(
            restored["items"],
            serde_json::json!([0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11])
        );
        assert_eq!(restored[""], "empty key");
        assert!(
            apply_response_extensions(
                serde_json::json!({}),
                &BTreeMap::from([("/invalid~escape".to_owned(), Value::Null)]),
            )
            .is_err()
        );
    }
}
