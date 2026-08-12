use axum::http::StatusCode;
use olp_db::oidc::OidcError;
use olp_engine::providers::OidcNetworkError;
use tracing::{error, warn};

use crate::{
    Problem,
    management::{map_persistence, reauthentication_required},
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

pub(super) fn authenticated_flow_session_changed() -> Problem {
    Problem::forbidden(
        "oidc_flow_session_changed",
        "Sign in with the exact session that started this security operation.",
    )
}

pub(super) fn is_authenticated_flow_session_changed(problem: &Problem) -> bool {
    problem.problem_type.as_ref() == "https://openllmproxy.dev/problems/oidc_flow_session_changed"
}

pub(super) fn invalid_id_token() -> Problem {
    Problem::unauthorized("The ID token is invalid.")
}

pub(super) fn field_problem(field: &str, detail: &str) -> Problem {
    Problem::field_validation(field, detail)
}

pub(super) fn oidc_not_configured() -> Problem {
    Problem::new(
        StatusCode::NOT_FOUND,
        "oidc_not_configured",
        "OIDC not configured",
        "OIDC has not been configured for this installation.",
    )
}

pub(crate) fn map_oidc(error: OidcError) -> Problem {
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
        OidcError::FlowSessionMismatch => authenticated_flow_session_changed(),
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

#[cfg(test)]
mod tests {
    use super::*;

    fn code(problem: &Problem) -> &str {
        problem
            .problem_type
            .strip_prefix("https://openllmproxy.dev/problems/")
            .unwrap()
    }

    #[test]
    fn oidc_domain_failures_have_stable_http_semantics() {
        let cases = [
            (
                OidcError::Invalid("bad".to_owned()),
                422,
                "validation_failed",
            ),
            (OidcError::NotConfigured, 404, "oidc_not_configured"),
            (OidcError::Disabled, 404, "oidc_not_configured"),
            (OidcError::PreconditionRequired, 428, "if_match_required"),
            (OidcError::PreconditionFailed, 412, "etag_mismatch"),
            (OidcError::FlowUnavailable, 400, "oidc_flow_unavailable"),
            (
                OidcError::FlowSessionMismatch,
                403,
                "oidc_flow_session_changed",
            ),
            (OidcError::FlowCapacity, 503, "oidc_flow_capacity_exhausted"),
            (OidcError::FlowRateLimited, 429, "oidc_flow_rate_limited"),
            (
                OidcError::IdentityAlreadyLinked,
                409,
                "oidc_identity_already_linked",
            ),
            (OidcError::IdentityNotFound, 404, "oidc_identity_not_found"),
            (
                OidcError::LastAuthenticationMethod,
                409,
                "last_authentication_method",
            ),
            (OidcError::LinkRequired, 409, "oidc_explicit_link_required"),
            (
                OidcError::ProvisioningDenied,
                403,
                "oidc_provisioning_denied",
            ),
            (OidcError::InactiveUser, 403, "account_inactive"),
            (
                OidcError::RecentAuthenticationRequired,
                428,
                "reauthentication_required",
            ),
            (
                OidcError::SessionUnavailable,
                401,
                "authentication_required",
            ),
            (
                OidcError::ReauthenticationIdentityMismatch,
                403,
                "oidc_reauthentication_identity_mismatch",
            ),
            (OidcError::Corrupt, 500, "internal_error"),
        ];

        for (error, status, expected_code) in cases {
            let problem = map_oidc(error);
            assert_eq!(problem.status, status, "wrong status for {expected_code}");
            assert_eq!(code(&problem), expected_code);
        }
    }

    #[test]
    fn flow_completion_translates_changed_configuration_to_a_restartable_error() {
        for error in [
            OidcError::NotConfigured,
            OidcError::Disabled,
            OidcError::PreconditionFailed,
        ] {
            let problem = map_oidc_flow_completion(error);
            assert_eq!(problem.status, 400);
            assert_eq!(code(&problem), "oidc_flow_stale");
        }
        assert_eq!(
            code(&map_oidc_flow_completion(OidcError::FlowUnavailable)),
            "oidc_flow_unavailable"
        );
    }

    #[test]
    fn network_failures_expose_only_generic_client_details() {
        let discovery_error = OidcNetworkError::ForbiddenAddress;
        let discovery_private_detail = discovery_error.to_string();
        let discovery = map_discovery_network(discovery_error);
        assert_eq!(discovery.status, 422);
        assert_eq!(code(&discovery), "validation_failed");
        assert_eq!(discovery.detail.as_ref(), "One or more fields are invalid.");
        assert_eq!(
            discovery.errors.get("discovery_url").unwrap(),
            &["Discovery, endpoint safety validation, or JWKS retrieval failed.".to_owned()]
        );
        assert!(
            !serde_json::to_string(&discovery)
                .unwrap()
                .contains(&discovery_private_detail)
        );

        let token_error = OidcNetworkError::ResponseTimeout;
        let token_private_detail = token_error.to_string();
        let token = map_token_network(token_error);
        assert_eq!(token.status, 503);
        assert_eq!(code(&token), "oidc_provider_unavailable");
        assert_eq!(
            token.detail.as_ref(),
            "A required service is temporarily unavailable."
        );
        assert!(token.errors.is_empty());
        assert!(
            !serde_json::to_string(&token)
                .unwrap()
                .contains(&token_private_detail)
        );
    }
}
