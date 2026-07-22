use axum::http::StatusCode;
use olp_providers::OidcNetworkError;
use olp_storage::OidcError;
use tracing::{error, warn};

use crate::{
    FieldErrors, Problem,
    management_api::{map_persistence, reauthentication_required},
};

pub(super) fn invalid_login_flow_cookie() -> Problem {
    Problem::bad_request(
        "oidc_login_flow_invalid",
        "The OIDC login flow is invalid or expired.",
    )
}

pub(super) fn invalid_callback() -> Problem {
    Problem::bad_request(
        "oidc_callback_invalid",
        "The authorization callback parameters are invalid.",
    )
}

pub(super) fn invalid_id_token() -> Problem {
    Problem::unauthorized("The ID token is invalid.")
}

pub(super) fn field_problem(field: &str, detail: &str) -> Problem {
    let mut errors = FieldErrors::new();
    errors.insert(field.to_owned(), vec![detail.to_owned()]);
    Problem::validation(errors)
}

pub(super) fn oidc_not_configured() -> Problem {
    Problem::new(
        StatusCode::NOT_FOUND,
        "oidc_not_configured",
        "OIDC not configured",
        "OIDC has not been configured for this installation.",
    )
}

pub(super) fn map_oidc(error: OidcError) -> Problem {
    match error {
        OidcError::Persistence(error) => map_persistence(error),
        OidcError::Invalid(detail) => field_problem("oidc", &detail),
        OidcError::NotConfigured | OidcError::Disabled => oidc_not_configured(),
        OidcError::PreconditionRequired => Problem::new(
            StatusCode::PRECONDITION_REQUIRED,
            "if_match_required",
            "Precondition required",
            "Supply the current OIDC configuration ETag in If-Match.",
        ),
        OidcError::PreconditionFailed => Problem::new(
            StatusCode::PRECONDITION_FAILED,
            "etag_mismatch",
            "Precondition failed",
            "The OIDC configuration changed after it was loaded. Refresh and retry.",
        ),
        OidcError::FlowUnavailable => Problem::bad_request(
            "oidc_flow_unavailable",
            "The authorization flow is invalid, expired, or already consumed.",
        ),
        OidcError::FlowCapacity => Problem::service_unavailable("oidc_flow_capacity_exhausted"),
        OidcError::FlowRateLimited => Problem::new(
            StatusCode::TOO_MANY_REQUESTS,
            "oidc_flow_rate_limited",
            "Too many OIDC authorization attempts",
            "Too many OIDC authorization flows were started. Wait before retrying.",
        ),
        OidcError::IdentityAlreadyLinked => Problem::conflict(
            "oidc_identity_already_linked",
            "This OIDC identity or local account is already linked.",
        ),
        OidcError::IdentityNotFound => Problem::new(
            StatusCode::NOT_FOUND,
            "oidc_identity_not_found",
            "OIDC identity not found",
            "The requested OIDC identity is not linked to the current account.",
        ),
        OidcError::LastAuthenticationMethod => Problem::conflict(
            "last_authentication_method",
            "Add a local password or another OIDC identity before unlinking this identity.",
        ),
        OidcError::LinkRequired => Problem::conflict(
            "oidc_explicit_link_required",
            "A local account with this email already exists. Sign in locally and explicitly link it.",
        ),
        OidcError::ProvisioningDenied => Problem::forbidden(
            "oidc_provisioning_denied",
            "This identity does not match an OIDC role mapping.",
        ),
        OidcError::InactiveUser => {
            Problem::forbidden("account_inactive", "The linked local account is inactive.")
        }
        OidcError::RecentAuthenticationRequired => reauthentication_required(),
        OidcError::SessionUnavailable => Problem::unauthorized(
            "The initiating session is missing, expired, or no longer current.",
        ),
        OidcError::ReauthenticationIdentityMismatch => Problem::forbidden(
            "oidc_reauthentication_identity_mismatch",
            "Fresh provider authentication did not match an identity linked to this account.",
        ),
        OidcError::Corrupt => {
            error!("stored OIDC data is invalid");
            Problem::internal()
        }
    }
}

pub(super) fn map_oidc_flow_completion(error: OidcError) -> Problem {
    match error {
        OidcError::NotConfigured | OidcError::Disabled | OidcError::PreconditionFailed => {
            Problem::bad_request(
                "oidc_flow_stale",
                "The OIDC configuration changed. Start authorization again.",
            )
        }
        other => map_oidc(other),
    }
}

pub(super) fn map_discovery_network(error: OidcNetworkError) -> Problem {
    warn!(%error, "OIDC discovery validation failed");
    field_problem(
        "discovery_url",
        "Discovery, endpoint safety validation, or JWKS retrieval failed.",
    )
}

pub(super) fn map_token_network(error: OidcNetworkError) -> Problem {
    warn!(%error, "OIDC provider request failed");
    Problem::service_unavailable("oidc_provider_unavailable")
}
