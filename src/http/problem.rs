use std::collections::BTreeMap;

use axum::Json;
use axum::http::StatusCode;
use axum::http::Uri;
use axum::http::header;
use axum::response::IntoResponse;
use axum::response::Response;
use serde::Deserialize;
use serde::Serialize;
use utoipa::ToSchema;

pub(crate) type FieldErrors = BTreeMap<String, Vec<FieldError>>;

#[derive(Debug, Clone, Serialize, Deserialize, ToSchema, PartialEq, Eq)]
pub(crate) struct FieldError {
    pub code: String,
    pub message: String,
}

impl From<String> for FieldError {
    fn from(message: String) -> Self {
        Self {
            code: "invalid".to_owned(),
            message,
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize, ToSchema)]
pub(crate) struct Problem {
    #[serde(rename = "type")]
    pub(crate) problem_type: Box<str>,
    pub(crate) title: Box<str>,
    pub(crate) status: u16,
    pub(crate) detail: Box<str>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub(crate) instance: Option<Box<str>>,
    /// Human-readable validation messages, keyed by field name.
    #[serde(skip_serializing_if = "BTreeMap::is_empty", default)]
    pub(crate) errors: Box<FieldErrors>,
}

impl Problem {
    pub(crate) fn new(
        status: StatusCode,
        code: &str,
        title: impl Into<String>,
        detail: impl Into<String>,
    ) -> Self {
        Self {
            problem_type: format!("https://openllmproxy.dev/problems/{code}").into_boxed_str(),
            title: title.into().into_boxed_str(),
            status: status.as_u16(),
            detail: detail.into().into_boxed_str(),
            instance: None,
            errors: Box::default(),
        }
    }

    pub(crate) fn bad_request(code: &str, detail: impl Into<String>) -> Self {
        Self::new(StatusCode::BAD_REQUEST, code, "Invalid request", detail)
    }

    pub(crate) fn validation(errors: FieldErrors) -> Self {
        let mut problem = Self::new(
            StatusCode::UNPROCESSABLE_ENTITY,
            "validation_failed",
            "Validation failed",
            "One or more fields are invalid.",
        );
        problem.errors = Box::new(errors);
        problem
    }

    pub(crate) fn field_validation(field: impl Into<String>, detail: impl Into<String>) -> Self {
        let mut errors = FieldErrors::new();
        errors.insert(
            field.into(),
            vec![FieldError {
                code: "invalid".to_owned(),
                message: detail.into(),
            }],
        );
        Self::validation(errors)
    }

    pub(crate) fn unauthorized(detail: impl Into<String>) -> Self {
        Self::new(
            StatusCode::UNAUTHORIZED,
            "authentication_required",
            "Authentication required",
            detail,
        )
    }

    pub(crate) fn forbidden(code: &str, detail: impl Into<String>) -> Self {
        Self::new(StatusCode::FORBIDDEN, code, "Forbidden", detail)
    }

    pub(crate) fn conflict(code: &str, detail: impl Into<String>) -> Self {
        Self::new(StatusCode::CONFLICT, code, "Conflict", detail)
    }

    pub(crate) fn service_unavailable(code: &str) -> Self {
        Self::new(
            StatusCode::SERVICE_UNAVAILABLE,
            code,
            "Service unavailable",
            "A required service is temporarily unavailable.",
        )
    }

    pub(crate) fn internal() -> Self {
        Self::new(
            StatusCode::INTERNAL_SERVER_ERROR,
            "internal_error",
            "Internal error",
            "The request could not be completed.",
        )
    }

    pub(crate) fn with_instance(mut self, uri: &Uri) -> Self {
        self.instance = Some(uri.path().to_owned().into_boxed_str());
        self
    }
}

impl IntoResponse for Problem {
    fn into_response(self) -> Response {
        let status = StatusCode::from_u16(self.status).unwrap_or(StatusCode::INTERNAL_SERVER_ERROR);
        let mut response = (status, Json(self)).into_response();
        response.headers_mut().insert(
            header::CONTENT_TYPE,
            header::HeaderValue::from_static("application/problem+json"),
        );
        response
    }
}

#[cfg(test)]
mod tests {
    use crate::http::problem::*;

    #[test]
    fn problem_instance_omits_query_parameters() {
        let uri: Uri = "/api/v3/providers?credential=must-not-be-reflected"
            .parse()
            .unwrap();
        let problem = Problem::bad_request("example", "example").with_instance(&uri);

        assert_eq!(problem.instance.as_deref(), Some("/api/v3/providers"));
    }

    #[test]
    fn field_validation_builds_the_standard_problem() {
        let problem = Problem::field_validation("model", "A model is required.");

        assert_eq!(problem.status, StatusCode::UNPROCESSABLE_ENTITY.as_u16());
        assert_eq!(
            problem.errors.get("model").map(|errors| errors
                .iter()
                .map(|error| error.message.clone())
                .collect::<Vec<_>>()),
            Some(vec!["A model is required.".to_owned()])
        );
    }
}
