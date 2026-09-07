use std::convert::Infallible;
use std::net::SocketAddr;

use crate::database::RequestProvenance;
use axum::extract::ConnectInfo;
use axum::extract::FromRequestParts;
use axum::http::request::Parts;

use crate::http::control::state::ManagementState;
use crate::http::proxy::audit_request_provenance;

pub(crate) struct Provenance(pub(crate) RequestProvenance);

impl FromRequestParts<ManagementState> for Provenance {
    type Rejection = Infallible;

    async fn from_request_parts(
        parts: &mut Parts,
        state: &ManagementState,
    ) -> Result<Self, Self::Rejection> {
        let peer = parts
            .extensions
            .get::<ConnectInfo<SocketAddr>>()
            .map(|ConnectInfo(peer)| *peer);
        Ok(Self(audit_request_provenance(
            &state.request_boundary,
            &parts.headers,
            peer,
        )))
    }
}
