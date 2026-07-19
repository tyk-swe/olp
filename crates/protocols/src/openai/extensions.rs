use std::collections::BTreeMap;

use serde::{Serialize, de::DeserializeOwned};
use serde_json::Value;

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

pub(crate) fn unescape_json_pointer(value: &str) -> String {
    value.replace("~1", "/").replace("~0", "~")
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
        if field.is_empty() || field.contains('/') {
            return Err(pointer.clone());
        }
        extra.insert(unescape_json_pointer(field), value.clone());
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
