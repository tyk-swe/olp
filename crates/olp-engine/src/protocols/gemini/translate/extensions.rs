use std::collections::BTreeMap;

use serde_json::Value;

pub(in crate::protocols) use crate::protocols::extensions::collect_extra;
use crate::protocols::extensions::{PointerExtensionError, apply_request_extensions};

use super::super::dto::{GenerateContentRequest, Part};
use super::errors::EncodeError;

pub(super) fn apply_extensions(
    request: &mut GenerateContentRequest,
    extensions: &BTreeMap<String, Value>,
) -> Result<(), EncodeError> {
    let mut insertions = extensions
        .iter()
        .filter_map(|(path, value)| parse_content_part_path(path).map(|indexes| (indexes, value)))
        .collect::<Vec<_>>();
    insertions.sort_by_key(|((content_index, part_index), _)| (*content_index, *part_index));
    for ((content_index, part_index), value) in insertions {
        let content = request.contents.get_mut(content_index).ok_or_else(|| {
            EncodeError::InvalidExtensionPath(format!(
                "/contents/{content_index}/parts/{part_index}"
            ))
        })?;
        if part_index > content.parts.len() {
            return Err(EncodeError::InvalidExtensionPath(format!(
                "/contents/{content_index}/parts/{part_index}"
            )));
        }
        let part: Part = serde_json::from_value(value.clone()).map_err(EncodeError::Json)?;
        content.parts.insert(part_index, part);
    }
    let remaining = extensions
        .iter()
        .filter(|(path, _)| parse_content_part_path(path).is_none())
        .map(|(path, value)| (path.clone(), value.clone()))
        .collect::<BTreeMap<_, _>>();
    apply_request_extensions(request, &remaining).map_err(|error| match error {
        PointerExtensionError::InvalidPath(path) => EncodeError::InvalidExtensionPath(path),
        PointerExtensionError::Json(error) => EncodeError::Json(error),
    })
}

pub(super) fn native_part_index(
    content_index: usize,
    canonical_part_index: usize,
    extensions: &BTreeMap<String, Value>,
) -> usize {
    let mut native_part_index = canonical_part_index;
    let mut preserved_part_indexes = extensions
        .keys()
        .filter_map(|path| parse_content_part_path(path))
        .filter_map(|(candidate_content_index, part_index)| {
            (candidate_content_index == content_index).then_some(part_index)
        })
        .collect::<Vec<_>>();
    preserved_part_indexes.sort_unstable();
    for preserved_part_index in preserved_part_indexes {
        if preserved_part_index <= native_part_index {
            native_part_index += 1;
        }
    }
    native_part_index
}

fn parse_content_part_path(path: &str) -> Option<(usize, usize)> {
    let segments = path.strip_prefix('/')?.split('/').collect::<Vec<_>>();
    match segments.as_slice() {
        ["contents", content_index, "parts", part_index] => {
            Some((content_index.parse().ok()?, part_index.parse().ok()?))
        }
        _ => None,
    }
}
