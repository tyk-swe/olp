use std::collections::BTreeMap;

use serde_json::Map;
use serde_json::Value;

use crate::protocols::openai::extensions::escape_json_pointer;

pub(crate) fn collect_object_extra(
    prefix: &str,
    object: Map<String, Value>,
    extensions: &mut BTreeMap<String, Value>,
) {
    for (field, value) in object {
        extensions.insert(format!("{prefix}/{}", escape_json_pointer(&field)), value);
    }
}
