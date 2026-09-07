use crate::access::authentication::SessionPrincipal;
use axum::extract::FromRequestParts;
use axum::http::request::Parts;

use crate::http::control::state::ManagementState;
use crate::http::problem::Problem;

use crate::access::sessions::require_mutation_session;
use crate::access::sessions::require_read_session;

/// The authenticated session behind a read-only management request. Rejects
/// with 401 when the session cookie is missing or expired.
pub(crate) struct ReadPrincipal(pub(crate) SessionPrincipal);

impl FromRequestParts<ManagementState> for ReadPrincipal {
    type Rejection = Problem;

    async fn from_request_parts(
        parts: &mut Parts,
        state: &ManagementState,
    ) -> Result<Self, Self::Rejection> {
        Ok(Self(require_read_session(state, &parts.headers).await?))
    }
}

/// The authenticated session behind a mutating management request: the
/// session plus the origin and CSRF checks a state change requires.
pub(crate) struct MutationPrincipal(pub(crate) SessionPrincipal);

impl FromRequestParts<ManagementState> for MutationPrincipal {
    type Rejection = Problem;

    async fn from_request_parts(
        parts: &mut Parts,
        state: &ManagementState,
    ) -> Result<Self, Self::Rejection> {
        Ok(Self(require_mutation_session(state, &parts.headers).await?))
    }
}
