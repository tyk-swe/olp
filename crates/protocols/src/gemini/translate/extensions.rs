use std::collections::BTreeMap;

use serde_json::Value;

use super::super::dto::GenerateContentRequest;
use super::errors::EncodeError;

pub(crate) fn collect_extra(
    prefix: &str,
    extra: &BTreeMap<String, Value>,
    extensions: &mut BTreeMap<String, Value>,
) {
    for (key, value) in extra {
        let key = key.replace('~', "~0").replace('/', "~1");
        extensions.insert(format!("{prefix}/{key}"), value.clone());
    }
}

pub(super) fn apply_extensions(
    request: &mut GenerateContentRequest,
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
        .ok_or_else(|| EncodeError::InvalidExtensionPath(pointer.into()))?
        .split('/')
        .map(|segment| segment.replace("~1", "/").replace("~0", "~"))
        .collect::<Vec<_>>();
    if segments.is_empty() || segments.len() > 16 {
        return Err(EncodeError::InvalidExtensionPath(pointer.into()));
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
                    .map_err(|_| EncodeError::InvalidExtensionPath(pointer.into()))?;
                if insert_array_item && index <= array.len() {
                    array.insert(index, value);
                    return Ok(());
                }
                let slot = array
                    .get_mut(index)
                    .ok_or_else(|| EncodeError::InvalidExtensionPath(pointer.into()))?;
                *slot = value;
                return Ok(());
            }
            Value::Object(object) => {
                current = object
                    .get_mut(segment)
                    .ok_or_else(|| EncodeError::InvalidExtensionPath(pointer.into()))?;
            }
            Value::Array(array) => {
                let index = segment
                    .parse::<usize>()
                    .map_err(|_| EncodeError::InvalidExtensionPath(pointer.into()))?;
                current = array
                    .get_mut(index)
                    .ok_or_else(|| EncodeError::InvalidExtensionPath(pointer.into()))?;
            }
            _ => return Err(EncodeError::InvalidExtensionPath(pointer.into())),
        }
    }
    Err(EncodeError::InvalidExtensionPath(pointer.into()))
}
