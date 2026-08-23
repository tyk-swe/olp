use axum::http::StatusCode;
use olp_db::{
    access::Error as AccessError, configuration::Error as ConfigurationError,
    error::Error as PersistenceError, identity::Error as IdentityError,
};
use tracing::error;

use crate::public_http::problem::Problem;

use super::sessions::reauthentication_required;

pub(crate) fn etag_mismatch(resource: &str) -> Problem {
    Problem::new(
        StatusCode::PRECONDITION_FAILED,
        "etag_mismatch",
        "Precondition failed",
        format!("The {resource} changed after it was loaded. Refresh and retry."),
    )
}

pub(crate) fn idempotency_key_reused() -> Problem {
    Problem::conflict(
        "idempotency_key_reused",
        "This Idempotency-Key has already been used for this operation.",
    )
}

pub(crate) fn idempotency_in_progress() -> Problem {
    Problem::conflict(
        "idempotency_in_progress",
        "An operation with this Idempotency-Key is still in progress.",
    )
}

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
        ConfigurationError::PreconditionFailed => etag_mismatch("provider"),
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
        ConfigurationError::IdempotencyConflict => idempotency_key_reused(),
        ConfigurationError::IdempotencyInProgress => idempotency_in_progress(),
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
        AccessError::PreconditionFailed => etag_mismatch("API key"),
        AccessError::IdempotencyConflict => idempotency_key_reused(),
        AccessError::IdempotencyInProgress => idempotency_in_progress(),
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
        IdentityError::PreconditionFailed => etag_mismatch("user"),
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
        IdentityError::IdempotencyConflict => idempotency_key_reused(),
        IdentityError::IdempotencyInProgress => idempotency_in_progress(),
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

#[cfg(test)]
mod tests {
    use super::*;

    fn assert_problem(problem: Problem, status: u16, code: &str) {
        assert_eq!(problem.status, status, "wrong status for {code}");
        assert_eq!(
            problem.problem_type.as_ref(),
            format!("https://openllmproxy.dev/problems/{code}")
        );
    }

    #[test]
    fn configuration_failures_retain_actionable_status_and_problem_codes() {
        let cases = [
            (
                ConfigurationError::ProviderNotFound,
                404,
                "provider_not_found",
            ),
            (
                ConfigurationError::ProviderIncomplete,
                422,
                "validation_failed",
            ),
            (ConfigurationError::PreconditionFailed, 412, "etag_mismatch"),
            (
                ConfigurationError::RouteNotFound,
                404,
                "route_draft_not_found",
            ),
            (
                ConfigurationError::RouteNotValidated,
                409,
                "route_not_validated",
            ),
            (
                ConfigurationError::InvalidRoute("invalid route".to_owned()),
                422,
                "validation_failed",
            ),
            (ConfigurationError::InvalidCredential, 500, "internal_error"),
            (
                ConfigurationError::IdempotencyConflict,
                409,
                "idempotency_key_reused",
            ),
            (
                ConfigurationError::IdempotencyInProgress,
                409,
                "idempotency_in_progress",
            ),
            (
                ConfigurationError::NotFound,
                404,
                "configuration_resource_not_found",
            ),
            (
                ConfigurationError::InUse,
                409,
                "configuration_resource_in_use",
            ),
            (
                ConfigurationError::Invalid("invalid".to_owned()),
                422,
                "validation_failed",
            ),
            (
                ConfigurationError::ProviderRevisionDiffTooLarge {
                    dimension: "models",
                    maximum: 10,
                },
                422,
                "validation_failed",
            ),
        ];
        for (error, status, code) in cases {
            assert_problem(map_configuration(error), status, code);
        }
    }

    #[test]
    fn access_failures_distinguish_invalid_missing_stale_and_replayed_requests() {
        for (error, status, code) in [
            (
                AccessError::Invalid("invalid".to_owned()),
                422,
                "validation_failed",
            ),
            (AccessError::NotFound, 404, "api_key_not_found"),
            (AccessError::PreconditionFailed, 412, "etag_mismatch"),
            (
                AccessError::IdempotencyConflict,
                409,
                "idempotency_key_reused",
            ),
            (
                AccessError::IdempotencyInProgress,
                409,
                "idempotency_in_progress",
            ),
        ] {
            assert_problem(map_access(error), status, code);
        }
    }

    #[test]
    fn identity_failures_preserve_security_sensitive_distinctions() {
        let cases = [
            (
                IdentityError::Invalid("invalid".to_owned()),
                422,
                "validation_failed",
            ),
            (IdentityError::NotFound, 404, "identity_resource_not_found"),
            (IdentityError::PreconditionFailed, 412, "etag_mismatch"),
            (IdentityError::LastOwner, 409, "last_owner_required"),
            (
                IdentityError::EmailAlreadyMember,
                409,
                "email_already_member",
            ),
            (
                IdentityError::PendingInvitationExists,
                409,
                "pending_invitation_exists",
            ),
            (
                IdentityError::InvitationUnavailable,
                410,
                "invitation_unavailable",
            ),
            (IdentityError::SessionForbidden, 403, "permission_denied"),
            (IdentityError::CorruptIdentity, 500, "internal_error"),
            (
                IdentityError::IdempotencyConflict,
                409,
                "idempotency_key_reused",
            ),
            (
                IdentityError::IdempotencyInProgress,
                409,
                "idempotency_in_progress",
            ),
            (
                IdentityError::LocalPasswordUnavailable,
                403,
                "local_password_unavailable",
            ),
            (
                IdentityError::LocalPasswordAlreadyConfigured,
                409,
                "local_password_already_configured",
            ),
            (
                IdentityError::RecentAuthenticationRequired,
                428,
                "reauthentication_required",
            ),
            (
                IdentityError::SessionUnavailable,
                401,
                "authentication_required",
            ),
        ];
        for (error, status, code) in cases {
            assert_problem(map_identity(error), status, code);
        }
    }

    #[test]
    fn persistence_mapping_exposes_only_session_availability() {
        assert_problem(
            map_persistence(PersistenceError::SessionUnavailable),
            401,
            "authentication_required",
        );
        for error in [
            PersistenceError::InvalidSessionTtl,
            PersistenceError::InvalidRecentAuthentication,
        ] {
            assert_problem(map_persistence(error), 500, "internal_error");
        }
        for error in [
            PersistenceError::RuntimeOutboxLeadershipLost,
            PersistenceError::InvalidWorkerHealth,
            PersistenceError::InvalidRequestMetadataGap,
        ] {
            assert_problem(map_persistence(error), 503, "database_unavailable");
        }
    }

    #[test]
    fn user_not_found_is_resource_specific() {
        assert_problem(user_not_found(), 404, "user_not_found");
    }
}
