pub(in crate::management) mod create;
pub(in crate::management) mod credentials;
pub(in crate::management) mod manage;
pub(in crate::management) mod models;
pub(in crate::management) mod revisions;

use olp_engine::domain::provider_configuration::Violation;

use crate::public_http::problem::{FieldErrorCodes, FieldErrors};

/// Records provider configuration violations as field errors, keeping each
/// message's machine-readable code alongside it.
pub(in crate::management) fn record_violations(
    violations: Vec<Violation>,
    errors: &mut FieldErrors,
    codes: &mut FieldErrorCodes,
) {
    for violation in violations {
        let field = violation.field.as_str();
        errors
            .entry(field.to_owned())
            .or_default()
            .push(violation.detail.to_owned());
        codes
            .entry(field.to_owned())
            .or_default()
            .push(violation.code.as_str().to_owned());
    }
}
