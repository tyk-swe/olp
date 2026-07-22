//! Shared JSON Pointer reconstruction for source-protocol extension fields.

use std::collections::BTreeMap;

use serde::{Serialize, de::DeserializeOwned};
use serde_json::Value;

#[derive(Debug)]
pub(crate) enum ApplyExtensionsError {
    Json(serde_json::Error),
    InvalidPath(String),
}

pub(crate) fn collect_extra(
    prefix: &str,
    extra: &BTreeMap<String, Value>,
    extensions: &mut BTreeMap<String, Value>,
) {
    for (key, value) in extra {
        extensions.insert(format!("{prefix}/{}", escape_segment(key)), value.clone());
    }
}

pub(crate) fn apply_extensions<T>(
    request: &mut T,
    extensions: &BTreeMap<String, Value>,
    insertable_array_parents: &[&str],
) -> Result<(), ApplyExtensionsError>
where
    T: Serialize + DeserializeOwned,
{
    if extensions.is_empty() {
        return Ok(());
    }
    let mut value = serde_json::to_value(&*request).map_err(ApplyExtensionsError::Json)?;
    let mut insertions = extensions
        .iter()
        .filter(|(path, _)| is_array_item_path(path, insertable_array_parents))
        .collect::<Vec<_>>();
    insertions.sort_by_key(|(path, _)| array_path_key(path));
    for (path, extension) in insertions {
        set_pointer(&mut value, path, extension.clone(), true)?;
    }
    for (path, extension) in extensions {
        if !is_array_item_path(path, insertable_array_parents) {
            set_pointer(&mut value, path, extension.clone(), false)?;
        }
    }
    *request = serde_json::from_value(value).map_err(ApplyExtensionsError::Json)?;
    Ok(())
}

fn escape_segment(value: &str) -> String {
    value.replace('~', "~0").replace('/', "~1")
}

fn unescape_segment(value: &str) -> String {
    value.replace("~1", "/").replace("~0", "~")
}

fn is_array_item_path(path: &str, insertable_array_parents: &[&str]) -> bool {
    let segments = path.trim_start_matches('/').split('/').collect::<Vec<_>>();
    segments.len() == 2
        && insertable_array_parents.contains(&segments[0])
        && segments[1].parse::<usize>().is_ok()
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
) -> Result<(), ApplyExtensionsError> {
    let segments = pointer
        .strip_prefix('/')
        .ok_or_else(|| ApplyExtensionsError::InvalidPath(pointer.to_owned()))?
        .split('/')
        .map(unescape_segment)
        .collect::<Vec<_>>();
    if segments.is_empty() || segments.len() > 16 {
        return Err(ApplyExtensionsError::InvalidPath(pointer.to_owned()));
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
                    .map_err(|_| ApplyExtensionsError::InvalidPath(pointer.to_owned()))?;
                if insert_array_item && index <= array.len() {
                    array.insert(index, value);
                    return Ok(());
                }
                let slot = array
                    .get_mut(index)
                    .ok_or_else(|| ApplyExtensionsError::InvalidPath(pointer.to_owned()))?;
                *slot = value;
                return Ok(());
            }
            Value::Object(object) => {
                current = object
                    .get_mut(segment)
                    .ok_or_else(|| ApplyExtensionsError::InvalidPath(pointer.to_owned()))?;
            }
            Value::Array(array) => {
                let index = segment
                    .parse::<usize>()
                    .map_err(|_| ApplyExtensionsError::InvalidPath(pointer.to_owned()))?;
                current = array
                    .get_mut(index)
                    .ok_or_else(|| ApplyExtensionsError::InvalidPath(pointer.to_owned()))?;
            }
            _ => return Err(ApplyExtensionsError::InvalidPath(pointer.to_owned())),
        }
    }
    Err(ApplyExtensionsError::InvalidPath(pointer.to_owned()))
}

#[cfg(test)]
mod tests {
    use serde::{Deserialize, Serialize};

    use super::*;

    #[derive(Debug, Deserialize, Eq, PartialEq, Serialize)]
    struct Document {
        tools: Vec<Value>,
        extra: BTreeMap<String, Value>,
    }

    #[test]
    fn escapes_keys_and_reconstructs_array_items_before_nested_fields() {
        let mut extensions = BTreeMap::new();
        collect_extra(
            "/extra",
            &BTreeMap::from([("a/b~c".into(), Value::Bool(true))]),
            &mut extensions,
        );
        extensions.insert("/tools/0".into(), serde_json::json!({"name": "first"}));
        extensions.insert("/tools/1/description".into(), Value::String("second".into()));
        let mut document = Document {
            tools: vec![serde_json::json!({"name": "second"})],
            extra: BTreeMap::new(),
        };

        apply_extensions(&mut document, &extensions, &["tools"]).unwrap();

        assert_eq!(document.tools[0]["name"], "first");
        assert_eq!(document.tools[1]["description"], "second");
        assert_eq!(document.extra["a/b~c"], Value::Bool(true));
    }

    #[test]
    fn rejects_missing_parents_and_excessive_depth() {
        let mut document = Document {
            tools: Vec::new(),
            extra: BTreeMap::new(),
        };
        let missing = BTreeMap::from([("/missing/value".into(), Value::Null)]);
        assert!(matches!(
            apply_extensions(&mut document, &missing, &["tools"]),
            Err(ApplyExtensionsError::InvalidPath(_))
        ));
        let deep = format!("/{}", ["x"; 17].join("/"));
        assert!(matches!(
            apply_extensions(
                &mut document,
                &BTreeMap::from([(deep, Value::Null)]),
                &["tools"]
            ),
            Err(ApplyExtensionsError::InvalidPath(_))
        ));
    }
}
