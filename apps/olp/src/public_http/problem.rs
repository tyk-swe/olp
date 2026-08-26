use std::collections::BTreeMap;

use axum::{
    Json,
    http::{StatusCode, Uri, header},
    response::{IntoResponse, Response},
};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

pub(crate) type FieldErrors = BTreeMap<String, Vec<String>>;
/// Machine-readable classifications for the fields in [`FieldErrors`], so a
/// client can react to a rejection without parsing its prose. A field is
/// present only when at least one of its messages carries a code, and the
/// codes are positionally aligned with that field's messages: entry `i` here
/// classifies message `i` there. A message with no code is represented by an
/// empty string so the alignment survives.
pub(crate) type FieldErrorCodes = BTreeMap<String, Vec<String>>;

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
    /// Machine-readable codes for the messages in `errors`, keyed by the same
    /// field names and positionally aligned with them: `error_codes[field][i]`
    /// classifies `errors[field][i]`. An empty string means that message
    /// carries no code, so the two arrays for a field always have the same
    /// length once the field appears here at all.
    #[serde(skip_serializing_if = "BTreeMap::is_empty", default)]
    pub(crate) error_codes: Box<FieldErrorCodes>,
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
            error_codes: Box::default(),
        }
    }

    pub(crate) fn bad_request(code: &str, detail: impl Into<String>) -> Self {
        Self::new(StatusCode::BAD_REQUEST, code, "Invalid request", detail)
    }

    pub(crate) fn validation(errors: FieldErrors) -> Self {
        Self::coded_validation(errors, FieldErrorCodes::new())
    }

    /// Validation problem whose messages carry machine-readable codes.
    ///
    /// `codes` is positionally aligned with `errors`: for every field present
    /// in both, `codes[field][i]` classifies `errors[field][i]`, and an empty
    /// string stands in for a message that has no code. Callers that mix
    /// hand-written messages with coded ones must pad accordingly — see
    /// `management::configuration::providers::record_violations`.
    pub(crate) fn coded_validation(errors: FieldErrors, codes: FieldErrorCodes) -> Self {
        let mut problem = Self::new(
            StatusCode::UNPROCESSABLE_ENTITY,
            "validation_failed",
            "Validation failed",
            "One or more fields are invalid.",
        );
        problem.errors = Box::new(errors);
        problem.error_codes = Box::new(codes);
        problem
    }

    pub(crate) fn field_validation(field: impl Into<String>, detail: impl Into<String>) -> Self {
        let mut errors = FieldErrors::new();
        errors.insert(field.into(), vec![detail.into()]);
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
    use super::*;

    #[test]
    fn problem_instance_omits_query_parameters() {
        let uri: Uri = "/api/v1/providers?credential=must-not-be-reflected"
            .parse()
            .unwrap();
        let problem = Problem::bad_request("example", "example").with_instance(&uri);

        assert_eq!(problem.instance.as_deref(), Some("/api/v1/providers"));
    }

    #[test]
    fn field_validation_builds_the_standard_problem() {
        let problem = Problem::field_validation("model", "A model is required.");

        assert_eq!(problem.status, StatusCode::UNPROCESSABLE_ENTITY.as_u16());
        assert_eq!(
            problem.errors.get("model"),
            Some(&vec!["A model is required.".to_owned()])
        );
    }
}
