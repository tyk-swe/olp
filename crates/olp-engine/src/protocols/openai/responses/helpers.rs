use std::collections::BTreeMap;

use serde_json::{Map, Value};

use super::super::extensions::escape_json_pointer;

pub(super) fn collect_object_extra(
    prefix: &str,
    object: Map<String, Value>,
    extensions: &mut BTreeMap<String, Value>,
) {
    for (field, value) in object {
        extensions.insert(format!("{prefix}/{}", escape_json_pointer(&field)), value);
    }
}
