use std::collections::BTreeMap;

use axum::{
    Json,
    http::{StatusCode, Uri, header},
    response::{IntoResponse, Response},
};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

pub type FieldErrors = BTreeMap<String, Vec<String>>;

#[derive(Debug, Clone, Serialize, Deserialize, ToSchema)]
pub struct Problem {
    #[serde(rename = "type")]
    pub problem_type: Box<str>,
    pub title: Box<str>,
    pub status: u16,
    pub detail: Box<str>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub instance: Option<Box<str>>,
    #[serde(skip_serializing_if = "BTreeMap::is_empty", default)]
    pub errors: Box<FieldErrors>,
}

impl Problem {
    pub fn new(
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

    pub fn bad_request(code: &str, detail: impl Into<String>) -> Self {
        Self::new(StatusCode::BAD_REQUEST, code, "Invalid request", detail)
    }

    pub fn validation(errors: FieldErrors) -> Self {
        let mut problem = Self::new(
            StatusCode::UNPROCESSABLE_ENTITY,
            "validation_failed",
            "Validation failed",
            "One or more fields are invalid.",
        );
        problem.errors = Box::new(errors);
        problem
    }

    pub fn unauthorized(detail: impl Into<String>) -> Self {
        Self::new(
            StatusCode::UNAUTHORIZED,
            "authentication_required",
            "Authentication required",
            detail,
        )
    }

    pub fn forbidden(code: &str, detail: impl Into<String>) -> Self {
        Self::new(StatusCode::FORBIDDEN, code, "Forbidden", detail)
    }

    pub fn conflict(code: &str, detail: impl Into<String>) -> Self {
        Self::new(StatusCode::CONFLICT, code, "Conflict", detail)
    }

    pub fn service_unavailable(code: &str) -> Self {
        Self::new(
            StatusCode::SERVICE_UNAVAILABLE,
            code,
            "Service unavailable",
            "A required service is temporarily unavailable.",
        )
    }

    pub fn internal() -> Self {
        Self::new(
            StatusCode::INTERNAL_SERVER_ERROR,
            "internal_error",
            "Internal error",
            "The request could not be completed.",
        )
    }

    pub fn with_instance(mut self, uri: &Uri) -> Self {
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
    use super::*;

    #[test]
    fn problem_instance_omits_query_parameters() {
        let uri: Uri = "/api/v1/providers?credential=must-not-be-reflected"
            .parse()
            .unwrap();
        let problem = Problem::bad_request("example", "example").with_instance(&uri);

        assert_eq!(problem.instance.as_deref(), Some("/api/v1/providers"));
    }
}
