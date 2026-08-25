use axum::{Json, extract::rejection::JsonRejection};
use serde::{Deserialize, Deserializer};

use crate::public_http::problem::Problem;

/// Distinguishes an omitted JSON field from one the caller explicitly sent as
/// `null`: an absent field deserializes to `None`, `null` to `Some(None)`.
/// Merge-semantics patches need that difference to tell "leave this alone"
/// from "clear this".
pub(crate) fn explicit_null<'de, D, T>(deserializer: D) -> Result<Option<Option<T>>, D::Error>
where
    D: Deserializer<'de>,
    T: Deserialize<'de>,
{
    Option::deserialize(deserializer).map(Some)
}

pub(crate) fn json_payload<T>(payload: Result<Json<T>, JsonRejection>) -> Result<T, Problem> {
    payload.map(|Json(value)| value).map_err(|error| {
        let detail = json_error_location(&error).map_or_else(
            || "The JSON body is invalid.".to_owned(),
            |(line, column)| format!("The JSON body is invalid at line {line}, column {column}."),
        );
        Problem::bad_request("invalid_json", detail)
    })
}

fn json_error_location(error: &(dyn std::error::Error + 'static)) -> Option<(usize, usize)> {
    let mut current = Some(error);
    while let Some(source) = current {
        if let Some(error) = source.downcast_ref::<serde_json::Error>() {
            return Some((error.line(), error.column()));
        }
        if let Some(error) = source.downcast_ref::<serde_path_to_error::Error<serde_json::Error>>()
        {
            return Some((error.inner().line(), error.inner().column()));
        }
        current = source.source();
    }
    None
}

#[cfg(test)]
mod tests {
    use axum::Json;
    use serde::Deserialize;
    use serde_json::json;

    #[test]
    fn error_detail_exposes_location_without_source_content() {
        let rejection = Json::<serde_json::Value>::from_bytes(b"{\n\"secret\": ]").unwrap_err();
        let problem = super::json_payload::<serde_json::Value>(Err(rejection)).unwrap_err();
        assert_eq!(
            problem.detail.as_ref(),
            "The JSON body is invalid at line 2, column 11."
        );
        assert!(!problem.detail.contains("secret"));
    }

    #[derive(Deserialize)]
    struct Patch {
        #[serde(default, deserialize_with = "super::explicit_null")]
        limit: Option<Option<u32>>,
    }

    #[test]
    fn an_omitted_field_is_distinguishable_from_an_explicit_null() {
        assert_eq!(
            serde_json::from_value::<Patch>(json!({})).unwrap().limit,
            None
        );
        assert_eq!(
            serde_json::from_value::<Patch>(json!({ "limit": null }))
                .unwrap()
                .limit,
            Some(None)
        );
        assert_eq!(
            serde_json::from_value::<Patch>(json!({ "limit": 7 }))
                .unwrap()
                .limit,
            Some(Some(7))
        );
    }
}
