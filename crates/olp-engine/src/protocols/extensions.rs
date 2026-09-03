use std::collections::BTreeMap;

use serde::{Serialize, de::DeserializeOwned};
use serde_json::{Value, map::Entry};

use crate::domain::canonical::requests::is_delivery_only_extension;

const MAX_MATERIALIZED_ARRAY_INDEX: usize = 1_024;

pub(in crate::protocols) enum PointerExtensionError {
    InvalidPath(String),
    Json(serde_json::Error),
}

impl From<serde_json::Error> for PointerExtensionError {
    fn from(error: serde_json::Error) -> Self {
        Self::Json(error)
    }
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

/// Reports a response pointer that could not be materialized. It carries
/// nothing: the only failure is the pointer itself, which every caller already
/// holds.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct UnresolvablePointer;

/// Sets `value` at `pointer` in a response document being assembled, creating
/// the missing objects and arrays along the way.
pub fn materialize_response_pointer(
    root: &mut Value,
    pointer: &str,
    value: Value,
) -> Result<(), UnresolvablePointer> {
    if !pointer.starts_with('/') || pointer.len() > 1_024 {
        return Err(UnresolvablePointer);
    }
    let segments = pointer[1..]
        .split('/')
        .map(unescape_json_pointer)
        .collect::<Vec<_>>();
    if segments.len() > 16 {
        return Err(UnresolvablePointer);
    }
    let mut current = root;
    for (index, segment) in segments.iter().enumerate() {
        if index + 1 == segments.len() {
            match current {
                Value::Object(object) => {
                    object.insert(segment.clone(), value);
                    return Ok(());
                }
                Value::Array(array) => {
                    let position = materialized_array_index(segment).ok_or(UnresolvablePointer)?;
                    while array.len() <= position {
                        array.push(Value::Null);
                    }
                    array[position] = value;
                    return Ok(());
                }
                _ => return Err(UnresolvablePointer),
            }
        }
        current = match current {
            Value::Object(object) => match object.entry(segment.clone()) {
                Entry::Occupied(entry) => entry.into_mut(),
                Entry::Vacant(entry) => {
                    entry.insert(empty_materialized_container(&segments[index + 1])?)
                }
            },
            Value::Array(array) => {
                let position = materialized_array_index(segment).ok_or(UnresolvablePointer)?;
                if array.len() <= position {
                    let next_value = empty_materialized_container(&segments[index + 1])?;
                    while array.len() <= position {
                        let mut next_value = next_value.clone();
                        if let Value::Object(object) = &mut next_value {
                            object.insert("index".to_owned(), Value::from(array.len()));
                        }
                        array.push(next_value);
                    }
                }
                &mut array[position]
            }
            _ => return Err(UnresolvablePointer),
        };
    }
    Err(UnresolvablePointer)
}

fn materialized_array_index(segment: &str) -> Option<usize> {
    segment
        .parse::<usize>()
        .ok()
        .filter(|index| *index <= MAX_MATERIALIZED_ARRAY_INDEX)
}

fn empty_materialized_container(segment: &str) -> Result<Value, UnresolvablePointer> {
    match segment.parse::<usize>() {
        Ok(index) if index <= MAX_MATERIALIZED_ARRAY_INDEX => Ok(Value::Array(Vec::new())),
        Ok(_) => Err(UnresolvablePointer),
        Err(_) if !segment.is_empty() && segment.bytes().all(|byte| byte.is_ascii_digit()) => {
            Err(UnresolvablePointer)
        }
        Err(_) => Ok(Value::Object(Default::default())),
    }
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
    let deliverable = || {
        extensions
            .iter()
            .filter(|(path, _)| !is_delivery_only_extension(path))
    };
    if deliverable().next().is_none() {
        return Ok(());
    }
    let mut value = serde_json::to_value(&*request)?;
    let mut insertions = deliverable()
        .filter(|(path, _)| is_array_item_path(path))
        .collect::<Vec<_>>();
    insertions.sort_by_key(|(path, _)| array_path_key(path));
    for (path, extension) in insertions {
        set_request_pointer(&mut value, path, extension.clone(), true)?;
    }
    for (path, extension) in deliverable().filter(|(path, _)| !is_array_item_path(path)) {
        set_request_pointer(&mut value, path, extension.clone(), false)?;
    }
    *request = serde_json::from_value(value)?;
    Ok(())
}

/// Paths that name a whole array element rather than a field inside one. These
/// are inserted (shifting later elements) instead of overwriting, and their
/// parent array is created when the canonical request produced none.
fn is_array_item_path(path: &str) -> bool {
    let segments = path.trim_start_matches('/').split('/').collect::<Vec<_>>();
    match segments.as_slice() {
        ["tools", index] => index.parse::<usize>().is_ok(),
        ["messages", message, "content", index] | ["contents", message, "parts", index] => {
            message.parse::<usize>().is_ok() && index.parse::<usize>().is_ok()
        }
        _ => false,
    }
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
                let next_is_index = segments
                    .get(position + 1)
                    .is_some_and(|next| next.parse::<usize>().is_ok());
                // A request whose canonical form carried no tools at all still
                // has to accept a `/tools/0` extension: the encoder omits the
                // empty array, so the container is created on demand.
                if insert_array_item && next_is_index && !object.contains_key(segment) {
                    object.insert(segment.clone(), Value::Array(Vec::new()));
                }
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
    let mut value = serde_json::to_value(response)?;
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

#[cfg(test)]
mod tests {
    use serde::{Deserialize, Serialize};
    use serde_json::json;

    use super::*;

    #[derive(Debug, Deserialize, PartialEq, Serialize)]
    struct ClosedWire {
        count: u8,
    }

    #[test]
    fn pointer_escaping_and_collection_round_trip_field_names() {
        let extra = BTreeMap::from([
            ("plain".to_owned(), json!(1)),
            ("a/b~c".to_owned(), json!({"retained": true})),
        ]);
        let mut extensions = BTreeMap::new();
        collect_extra("/body", &extra, &mut extensions);

        assert_eq!(escape_json_pointer("a/b~c"), "a~1b~0c");
        assert_eq!(unescape_json_pointer("a~1b~0c"), "a/b~c");
        assert_eq!(extensions["/body/a~1b~0c"], json!({"retained": true}));
    }

    #[test]
    fn materialized_response_pointers_reject_oversized_array_indices() {
        for pointer in ["/choices/1025", "/choices/4294967295/vendor"] {
            let mut root = json!({"choices": []});
            assert_eq!(
                materialize_response_pointer(&mut root, pointer, json!(true)),
                Err(UnresolvablePointer)
            );
            assert_eq!(root, json!({"choices": []}));
        }

        for pointer in ["/items/1025/name", "/items/18446744073709551616/name"] {
            let mut root = json!({});
            assert_eq!(
                materialize_response_pointer(&mut root, pointer, json!(true)),
                Err(UnresolvablePointer)
            );
            assert_eq!(root, json!({}));
        }
    }

    #[test]
    fn materialized_response_pointers_allow_numeric_keys_in_existing_objects() {
        let mut root = json!({"items": {"1025": {}}});

        materialize_response_pointer(&mut root, "/items/1025/name", json!("kept")).unwrap();

        assert_eq!(root, json!({"items": {"1025": {"name": "kept"}}}));
    }

    #[test]
    fn flat_extensions_accept_only_noncolliding_top_level_fields() {
        let mut extra = BTreeMap::new();
        apply_flat_extensions(
            &mut extra,
            &BTreeMap::from([
                ("/simple".to_owned(), json!(1)),
                ("/a~1b~0c".to_owned(), json!(2)),
            ]),
        )
        .ok()
        .unwrap();
        assert_eq!(extra["simple"], 1);
        assert_eq!(extra["a/b~c"], 2);

        for path in ["", "missing-slash", "/", "/nested/value"] {
            assert_eq!(
                apply_flat_extensions(
                    &mut BTreeMap::new(),
                    &BTreeMap::from([(path.to_owned(), Value::Null)]),
                ),
                Err(path.to_owned())
            );
        }

        let mut root = json!({"known": true});
        assert_eq!(
            insert_flat_extension(&mut root, "/known", json!(false)),
            Err("/known".to_owned())
        );
        assert_eq!(
            insert_flat_extension(&mut json!([]), "/vendor", json!(1)),
            Err("/vendor".to_owned())
        );
    }

    #[test]
    fn pointer_extensions_restore_nested_object_fields_without_overwrite() {
        let wire = json!({
            "items": [{"known": 1}],
            "object": {"stable": true}
        });
        let restored = apply_pointer_extensions(
            wire,
            &BTreeMap::from([
                ("/items/0/a~1b".to_owned(), json!("array item")),
                ("/object/vendor".to_owned(), json!({"kept": true})),
            ]),
        )
        .ok()
        .unwrap();
        assert_eq!(restored["items"][0]["a/b"], "array item");
        assert_eq!(restored["object"]["vendor"], json!({"kept": true}));

        for path in [
            "",
            "no-leading-slash",
            "/missing/child",
            "/items/not-an-index/vendor",
            "/items/4/vendor",
            "/items/0/known",
            "/items/0/known/child",
        ] {
            assert_eq!(
                apply_pointer_extensions(
                    json!({"items": [{"known": 1}]}),
                    &BTreeMap::from([(path.to_owned(), json!(2))]),
                ),
                Err(path.to_owned()),
                "path {path} must fail closed"
            );
        }
    }

    #[test]
    fn request_extensions_insert_array_items_in_stable_index_order() {
        let mut request = json!({
            "tools": [{"name": "canonical"}],
            "metadata": {"known": true}
        });
        apply_request_extensions(
            &mut request,
            &BTreeMap::from([
                ("/tools/1".to_owned(), json!({"name": "extension-1"})),
                ("/tools/0".to_owned(), json!({"name": "extension-0"})),
                ("/metadata/vendor".to_owned(), json!("preserved")),
            ]),
        )
        .ok()
        .unwrap();

        assert_eq!(
            request["tools"],
            json!([
                {"name": "extension-0"},
                {"name": "extension-1"},
                {"name": "canonical"}
            ])
        );
        assert_eq!(request["metadata"]["vendor"], "preserved");
    }

    #[test]
    fn array_item_extensions_create_a_container_the_encoder_omitted() {
        // A request whose only tool is a server-side one has an empty canonical
        // `tools`, and the encoder skips serializing an empty array.
        let mut request = json!({"model": "upstream"});
        apply_request_extensions(
            &mut request,
            &BTreeMap::from([(
                "/tools/0".to_owned(),
                json!({"type": "web_search_20250305", "name": "web_search"}),
            )]),
        )
        .ok()
        .unwrap();
        assert_eq!(request["tools"][0]["type"], "web_search_20250305");

        // Message content items insert rather than overwrite, in index order.
        let mut request = json!({"messages": [{"content": [{"type": "text"}]}]});
        apply_request_extensions(
            &mut request,
            &BTreeMap::from([
                (
                    "/messages/0/content/0".to_owned(),
                    json!({"type": "thinking"}),
                ),
                (
                    "/messages/0/content/2".to_owned(),
                    json!({"type": "trailing"}),
                ),
            ]),
        )
        .ok()
        .unwrap();
        assert_eq!(
            request["messages"][0]["content"],
            json!([{"type": "thinking"}, {"type": "text"}, {"type": "trailing"}])
        );

        // A field path into a container that does not exist still fails closed.
        let mut request = json!({"model": "upstream"});
        assert!(matches!(
            apply_request_extensions(
                &mut request,
                &BTreeMap::from([("/tools/0/name".to_owned(), json!("web_search"))]),
            ),
            Err(PointerExtensionError::InvalidPath(path)) if path == "/tools/0/name"
        ));
    }

    #[test]
    fn request_extensions_reject_invalid_paths_and_invalid_wire_mutations() {
        let paths = [
            "missing-slash".to_owned(),
            "/missing/child".to_owned(),
            "/tools/not-an-index/name".to_owned(),
            "/tools/9/name".to_owned(),
            format!("/{}", vec!["level"; 17].join("/")),
        ];
        for path in paths {
            let mut request = json!({"tools": [{"name": "known"}]});
            assert!(matches!(
                apply_request_extensions(
                    &mut request,
                    &BTreeMap::from([(path.clone(), json!(1))]),
                ),
                Err(PointerExtensionError::InvalidPath(invalid)) if invalid == path
            ));
        }

        let mut request = ClosedWire { count: 1 };
        assert!(matches!(
            apply_request_extensions(
                &mut request,
                &BTreeMap::from([("/count".to_owned(), json!("not a number"))]),
            ),
            Err(PointerExtensionError::Json(_))
        ));
        assert_eq!(request, ClosedWire { count: 1 });
    }

    #[test]
    fn response_extensions_create_parent_array_items_before_children() {
        let restored = apply_response_extensions(
            json!({"output": []}),
            &BTreeMap::from([
                ("/output/0/vendor".to_owned(), json!("nested")),
                ("/output/0".to_owned(), json!({"type": "future"})),
                ("/top~1level".to_owned(), json!(true)),
            ]),
        )
        .ok()
        .unwrap();
        assert_eq!(restored["output"][0]["type"], "future");
        assert_eq!(restored["output"][0]["vendor"], "nested");
        assert_eq!(restored["top/level"], true);

        for path in [
            "",
            "/",
            "missing-slash",
            "/output/not-an-index",
            "/output/2",
            "/output/0/type",
        ] {
            assert!(matches!(
                apply_response_extensions(
                    json!({"output": [{"type": "known"}]}),
                    &BTreeMap::from([(path.to_owned(), json!(1))]),
                ),
                Err(PointerExtensionError::InvalidPath(invalid)) if invalid == path
            ));
        }
        let too_deep = format!("/{}", vec!["level"; 25].join("/"));
        assert!(matches!(
            apply_response_extensions(
                json!({}),
                &BTreeMap::from([(too_deep.clone(), json!(1))]),
            ),
            Err(PointerExtensionError::InvalidPath(invalid)) if invalid == too_deep
        ));
    }
}
