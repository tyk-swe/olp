use std::{convert::Infallible, net::SocketAddr};

use axum::extract::{ConnectInfo, FromRequestParts};
use axum::http::request::Parts;
use olp_db::store::RequestProvenance;

use crate::{
    bootstrap::mode_dependencies::ManagementState, public_http::proxy::audit_request_provenance,
};

/// Request-boundary attribution for the audit rows a handler writes. Handlers
/// that produce audit events pass it to `Store::with_provenance`; every other
/// path writes those columns as null.
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
            state.request_boundary(),
            &parts.headers,
            peer,
        )))
    }
}
