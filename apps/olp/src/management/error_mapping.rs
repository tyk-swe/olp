use axum::http::StatusCode;
use olp_db::{
    PersistenceError, access::AccessError, configuration::ConfigurationError,
    identity::IdentityError,
};
use tracing::error;

use crate::Problem;

use super::sessions::reauthentication_required;

pub(crate) fn map_configuration(error: ConfigurationError) -> Problem {
    match error {
        ConfigurationError::ProviderNotFound => Problem::new(
            StatusCode::NOT_FOUND,
            "provider_not_found",
            "Provider not found",
            "The provider does not exist.",
        ),
        ConfigurationError::ProviderIncomplete => Problem::field_validation(
            "provider",
            "A credential and enabled model are required before activation; OpenAI-compatible model capabilities must also be live-certified.",
        ),
        ConfigurationError::PreconditionFailed => Problem::new(
            StatusCode::PRECONDITION_FAILED,
            "etag_mismatch",
            "Precondition failed",
            "The provider changed after it was loaded. Refresh and retry.",
        ),
        ConfigurationError::Persistence(error) => map_persistence(error),
        ConfigurationError::RouteNotFound => Problem::new(
            StatusCode::NOT_FOUND,
            "route_draft_not_found",
            "Route draft not found",
            "The route draft does not exist.",
        ),
        ConfigurationError::RouteNotValidated => Problem::conflict(
            "route_not_validated",
            "Validate the route draft before activation.",
        ),
        ConfigurationError::InvalidRoute(detail) => Problem::field_validation("route", detail),
        ConfigurationError::RuntimeCompile(error) => {
            error!(%error, "runtime compilation failed");
            Problem::internal()
        }
        ConfigurationError::InvalidCredential => {
            error!("stored provider credential is malformed");
            Problem::internal()
        }
        ConfigurationError::IdempotencyConflict => Problem::conflict(
            "idempotency_key_reused",
            "This Idempotency-Key has already been used for this operation.",
        ),
        ConfigurationError::IdempotencyInProgress => Problem::conflict(
            "idempotency_in_progress",
            "An operation with this Idempotency-Key is still in progress.",
        ),
        ConfigurationError::NotFound => Problem::new(
            StatusCode::NOT_FOUND,
            "configuration_resource_not_found",
            "Resource not found",
            "The requested configuration resource does not exist.",
        ),
        ConfigurationError::InUse => Problem::conflict(
            "configuration_resource_in_use",
            "The resource is active or referenced and cannot be removed.",
        ),
        ConfigurationError::Invalid(detail) => Problem::field_validation("configuration", detail),
        ConfigurationError::ProviderRevisionDiffTooLarge { dimension, maximum } => {
            Problem::field_validation(
                "revisions",
                format!(
                    "provider revision diff supports at most {maximum} {dimension} per revision"
                ),
            )
        }
    }
}

pub(crate) fn map_access(error: AccessError) -> Problem {
    match error {
        AccessError::Persistence(error) => map_persistence(error),
        AccessError::RuntimeCompile(error) => {
            error!(%error, "runtime compilation failed after API key change");
            Problem::internal()
        }
        AccessError::Invalid(detail) => Problem::field_validation("api_key", detail),
        AccessError::NotFound => Problem::new(
            StatusCode::NOT_FOUND,
            "api_key_not_found",
            "API key not found",
            "The API key does not exist.",
        ),
        AccessError::PreconditionFailed => Problem::new(
            StatusCode::PRECONDITION_FAILED,
            "etag_mismatch",
            "Precondition failed",
            "The API key changed after it was loaded. Refresh and retry.",
        ),
        AccessError::IdempotencyConflict => Problem::conflict(
            "idempotency_key_reused",
            "This Idempotency-Key has already been used for that API key operation.",
        ),
        AccessError::IdempotencyInProgress => Problem::conflict(
            "idempotency_in_progress",
            "An operation with this Idempotency-Key is still in progress.",
        ),
    }
}

pub(crate) fn user_not_found() -> Problem {
    Problem::new(
        StatusCode::NOT_FOUND,
        "user_not_found",
        "User not found",
        "The user does not exist.",
    )
}

pub(crate) fn map_identity(error: IdentityError) -> Problem {
    match error {
        IdentityError::Persistence(error) => map_persistence(error),
        IdentityError::Invalid(detail) => Problem::field_validation("identity", detail),
        IdentityError::NotFound => Problem::new(
            StatusCode::NOT_FOUND,
            "identity_resource_not_found",
            "Identity resource not found",
            "The requested identity resource does not exist.",
        ),
        IdentityError::PreconditionFailed => Problem::new(
            StatusCode::PRECONDITION_FAILED,
            "etag_mismatch",
            "Precondition failed",
            "The user changed after it was loaded. Refresh and retry.",
        ),
        IdentityError::LastOwner => Problem::conflict(
            "last_owner_required",
            "The last active owner cannot be demoted.",
        ),
        IdentityError::EmailAlreadyMember => Problem::conflict(
            "email_already_member",
            "A user with this email already belongs to the installation.",
        ),
        IdentityError::PendingInvitationExists => Problem::conflict(
            "pending_invitation_exists",
            "A pending invitation already exists for this email.",
        ),
        IdentityError::InvitationUnavailable => Problem::new(
            StatusCode::GONE,
            "invitation_unavailable",
            "Invitation unavailable",
            "The invitation is invalid, expired, revoked, or already accepted.",
        ),
        IdentityError::SessionForbidden => Problem::forbidden(
            "permission_denied",
            "Only an owner can revoke another user's session.",
        ),
        IdentityError::CorruptIdentity => {
            error!("stored identity data contains an unknown role");
            Problem::internal()
        }
        IdentityError::IdempotencyConflict => Problem::conflict(
            "idempotency_key_reused",
            "This Idempotency-Key has already been used for this invitation operation.",
        ),
        IdentityError::IdempotencyInProgress => Problem::conflict(
            "idempotency_in_progress",
            "An operation with this Idempotency-Key is still in progress.",
        ),
        IdentityError::LocalPasswordUnavailable => Problem::forbidden(
            "local_password_unavailable",
            "This profile does not have a local password.",
        ),
        IdentityError::LocalPasswordAlreadyConfigured => Problem::conflict(
            "local_password_already_configured",
            "A local password is already configured. Use the password-change operation.",
        ),
        IdentityError::RecentAuthenticationRequired => reauthentication_required(),
        IdentityError::SessionUnavailable => Problem::unauthorized(
            "The session changed while the security operation was in progress.",
        ),
    }
}

pub(crate) fn map_persistence(error: PersistenceError) -> Problem {
    match error {
        PersistenceError::SessionUnavailable => {
            Problem::unauthorized("The session is missing, expired, or no longer current.")
        }
        PersistenceError::InvalidSessionTtl | PersistenceError::InvalidRecentAuthentication => {
            error!(%error, "invalid server authentication configuration");
            Problem::internal()
        }
        other => {
            error!(error = %other, "management persistence operation failed");
            Problem::service_unavailable("database_unavailable")
        }
    }
}
