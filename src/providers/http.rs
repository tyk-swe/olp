use crate::providers::validation::Violation;

use crate::http::problem::FieldErrors;

pub(crate) fn record_violations(violations: Vec<Violation>, errors: &mut FieldErrors) {
    for violation in violations {
        errors
            .entry(violation.field.as_str().to_owned())
            .or_default()
            .push(crate::http::problem::FieldError {
                code: violation.code.as_str().to_owned(),
                message: violation.detail.to_owned(),
            });
    }
}

pub mod create;
pub mod pool;

pub mod credentials;

pub mod manage;

pub mod models;

pub mod revisions;

pub(crate) fn router()
-> utoipa_axum::router::OpenApiRouter<crate::http::control::state::ManagementState> {
    utoipa_axum::router::OpenApiRouter::new()
        .routes(utoipa_axum::routes!(pool::provider_vendors))
        .routes(utoipa_axum::routes!(pool::credential_slots))
        .routes(utoipa_axum::routes!(pool::put_credential_slot))
        .routes(utoipa_axum::routes!(pool::validate_credential_slot))
        .routes(utoipa_axum::routes!(
            crate::providers::http::create::activate_provider
        ))
        .routes(utoipa_axum::routes!(
            crate::providers::http::create::create_provider
        ))
        .routes(utoipa_axum::routes!(
            crate::providers::http::credentials::list_provider_credentials
        ))
        .routes(utoipa_axum::routes!(
            crate::providers::http::credentials::revoke_provider_credential
        ))
        .routes(utoipa_axum::routes!(
            crate::providers::http::credentials::rotate_provider_credential
        ))
        .routes(utoipa_axum::routes!(
            crate::providers::http::manage::disable_provider
        ))
        .routes(utoipa_axum::routes!(
            crate::providers::http::manage::get_provider
        ))
        .routes(utoipa_axum::routes!(
            crate::providers::http::manage::list_providers
        ))
        .routes(utoipa_axum::routes!(
            crate::providers::http::manage::probe_provider
        ))
        .routes(utoipa_axum::routes!(
            crate::providers::http::manage::restore_provider_as_draft
        ))
        .routes(utoipa_axum::routes!(
            crate::providers::http::manage::update_provider
        ))
        .routes(utoipa_axum::routes!(
            crate::providers::http::models::certify_provider_model
        ))
        .routes(utoipa_axum::routes!(
            crate::providers::http::models::discover_provider_models
        ))
        .routes(utoipa_axum::routes!(
            crate::providers::http::models::list_provider_kind_capabilities
        ))
        .routes(utoipa_axum::routes!(
            crate::providers::http::models::list_provider_kinds
        ))
        .routes(utoipa_axum::routes!(
            crate::providers::http::models::list_provider_model_inventory
        ))
        .routes(utoipa_axum::routes!(
            crate::providers::http::models::list_provider_models
        ))
        .routes(utoipa_axum::routes!(
            crate::providers::http::models::set_provider_model
        ))
        .routes(utoipa_axum::routes!(
            crate::providers::http::revisions::diff_provider_revisions
        ))
        .routes(utoipa_axum::routes!(
            crate::providers::http::revisions::get_provider_revision
        ))
        .routes(utoipa_axum::routes!(
            crate::providers::http::revisions::list_provider_revision_models
        ))
        .routes(utoipa_axum::routes!(
            crate::providers::http::revisions::list_provider_revisions
        ))
        .routes(utoipa_axum::routes!(
            crate::providers::http::revisions::restore_provider_revision
        ))
}
