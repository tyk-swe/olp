pub(in crate::management) mod create;
pub(in crate::management) mod credentials;
pub(in crate::management) mod manage;
pub(in crate::management) mod models;
pub(in crate::management) mod revisions;

use olp_engine::domain::provider_configuration::Violation;

use crate::public_http::problem::{FieldErrorCodes, FieldErrors};

/// Records provider configuration violations as field errors, keeping each
/// message's machine-readable code at the same index in `codes`.
///
/// Handlers may already have pushed hand-written messages for a field before
/// the domain violations arrive, and those messages have no code. Padding
/// `codes` with empty strings up to the message being added keeps index `i` of
/// a field's codes paired with index `i` of its messages, which is the contract
/// clients rely on to map a code back to the message it explains.
pub(in crate::management) fn record_violations(
    violations: Vec<Violation>,
    errors: &mut FieldErrors,
    codes: &mut FieldErrorCodes,
) {
    for violation in violations {
        let field = violation.field.as_str();
        let messages = errors.entry(field.to_owned()).or_default();
        messages.push(violation.detail.to_owned());
        let codes = codes.entry(field.to_owned()).or_default();
        // Any earlier uncoded message for this field takes a blank code so the
        // two vectors stay index-aligned.
        codes.resize(messages.len() - 1, String::new());
        codes.push(violation.code.as_str().to_owned());
    }
}

pub(super) fn router() -> axum::Router<crate::management::state::ManagementState> {
    use axum::routing::{get, patch, post};
    axum::Router::new()
        .route("/api/v1/provider-kinds", get(models::list_provider_kinds))
        .route(
            "/api/v1/provider-kinds/{provider_kind}/capabilities",
            get(models::list_provider_kind_capabilities),
        )
        .route(
            "/api/v1/providers",
            get(manage::list_providers).post(create::create_provider),
        )
        .route(
            "/api/v1/provider-models",
            get(models::list_provider_model_inventory),
        )
        .route(
            "/api/v1/providers/{provider_id}",
            get(manage::get_provider).patch(manage::update_provider),
        )
        .route(
            "/api/v1/providers/{provider_id}/activate",
            post(create::activate_provider),
        )
        .route(
            "/api/v1/providers/{provider_id}/disable",
            post(manage::disable_provider),
        )
        .route(
            "/api/v1/providers/{provider_id}/restore-as-draft",
            post(manage::restore_provider_as_draft),
        )
        .route(
            "/api/v1/providers/{provider_id}/revisions",
            get(revisions::list_provider_revisions),
        )
        .route(
            "/api/v1/providers/{provider_id}/revisions/diff",
            get(revisions::diff_provider_revisions),
        )
        .route(
            "/api/v1/providers/{provider_id}/revisions/{revision_id}",
            get(revisions::get_provider_revision),
        )
        .route(
            "/api/v1/providers/{provider_id}/revisions/{revision_id}/models",
            get(revisions::list_provider_revision_models),
        )
        .route(
            "/api/v1/providers/{provider_id}/revisions/{revision_id}/restore-as-draft",
            post(revisions::restore_provider_revision),
        )
        .route(
            "/api/v1/providers/{provider_id}/credentials",
            get(credentials::list_provider_credentials)
                .post(credentials::rotate_provider_credential),
        )
        .route(
            "/api/v1/providers/{provider_id}/credentials/{credential_id}/revoke",
            post(credentials::revoke_provider_credential),
        )
        .route(
            "/api/v1/providers/{provider_id}/probe",
            post(manage::probe_provider),
        )
        .route(
            "/api/v1/providers/{provider_id}/discovery",
            post(models::discover_provider_models),
        )
        .route(
            "/api/v1/providers/{provider_id}/models/{model_id}",
            patch(models::set_provider_model),
        )
        .route(
            "/api/v1/providers/{provider_id}/models",
            get(models::list_provider_models),
        )
        .route(
            "/api/v1/providers/{provider_id}/models/{model_id}/certify",
            post(models::certify_provider_model),
        )
}

#[cfg(test)]
mod tests {
    use olp_engine::domain::provider_configuration::{
        ProviderViolationCode, ProviderViolationField, Violation,
    };

    use super::record_violations;
    use crate::public_http::problem::{FieldErrorCodes, FieldErrors};

    #[test]
    fn codes_stay_aligned_with_hand_written_messages() {
        let mut errors = FieldErrors::new();
        let mut codes = FieldErrorCodes::new();
        errors
            .entry("endpoint".to_owned())
            .or_default()
            .push("Use between 1 and 100 characters.".to_owned());

        record_violations(
            vec![
                Violation {
                    field: ProviderViolationField::Endpoint,
                    code: ProviderViolationCode::Required,
                    detail: "Endpoint is required.",
                },
                Violation {
                    field: ProviderViolationField::Endpoint,
                    code: ProviderViolationCode::Forbidden,
                    detail: "Endpoint is not supported.",
                },
            ],
            &mut errors,
            &mut codes,
        );

        let messages = &errors["endpoint"];
        let recorded = &codes["endpoint"];
        assert_eq!(messages.len(), recorded.len());
        assert_eq!(messages[0], "Use between 1 and 100 characters.");
        assert_eq!(recorded[0], "");
        assert_eq!(messages[1], "Endpoint is required.");
        assert_eq!(recorded[1], "required");
        assert_eq!(messages[2], "Endpoint is not supported.");
        assert_eq!(recorded[2], "forbidden");
    }

    #[test]
    fn no_violations_record_nothing() {
        let mut errors = FieldErrors::new();
        let mut codes = FieldErrorCodes::new();
        record_violations(Vec::new(), &mut errors, &mut codes);
        assert!(errors.is_empty());
        assert!(codes.is_empty());
    }
}
