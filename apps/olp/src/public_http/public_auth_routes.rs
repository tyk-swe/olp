//! Canonical public-authentication routes that require source admission.

use axum::http::Method;

/// A registered public-authentication operation whose source must be
/// established before any request-body extractor runs.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum PublicAuthRoute {
    FirstOwnerSetup,
    PasswordLogin,
    InvitationAcceptance,
    OidcLoginGet,
    OidcLoginPost,
}

impl PublicAuthRoute {
    pub(crate) const ALL: [Self; 5] = [
        Self::FirstOwnerSetup,
        Self::PasswordLogin,
        Self::InvitationAcceptance,
        Self::OidcLoginGet,
        Self::OidcLoginPost,
    ];

    pub(crate) const fn path(self) -> &'static str {
        match self {
            Self::FirstOwnerSetup => "/api/v1/setup",
            Self::PasswordLogin => "/api/v1/sessions",
            Self::InvitationAcceptance => "/api/v1/invitations/accept",
            Self::OidcLoginGet | Self::OidcLoginPost => "/api/v1/oidc/login",
        }
    }

    pub(crate) fn method(self) -> Method {
        match self {
            Self::OidcLoginGet => Method::GET,
            Self::FirstOwnerSetup
            | Self::PasswordLogin
            | Self::InvitationAcceptance
            | Self::OidcLoginPost => Method::POST,
        }
    }

    pub(crate) fn classify(method: &Method, path: &str) -> Option<Self> {
        Self::ALL
            .into_iter()
            .find(|route| route.method() == *method && route.path() == path)
    }
}
