use axum::{Json, extract::rejection::JsonRejection};

use crate::public_http::problem::Problem;

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
}
