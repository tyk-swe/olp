use axum::{
    Json,
    extract::{Path, State},
    http::{HeaderMap, StatusCode},
    response::{IntoResponse, Response},
};
use chrono::Utc;
use olp_storage::{OidcIdentityRecord, RecentAuthMaterial, SessionMaterial, UnlinkOidcIdentity};
use serde::Serialize;
use utoipa::ToSchema;
use uuid::Uuid;

use super::error::map_oidc;
use crate::{
    ApiState, Problem,
    management_api::{
        RECENT_AUTH_COOKIE, append_security_transition_cookies, cookie, map_persistence,
        reauthentication_required, require_mutation_session, require_read_session, require_store,
        validate_session_cookie_ttl,
    },
};

#[derive(Clone, Debug, Serialize, ToSchema)]
pub struct OidcIdentityResponse {
    pub id: Uuid,
    pub issuer: String,
    pub email_at_link: Option<String>,
    pub last_login_at: Option<chrono::DateTime<Utc>>,
    pub created_at: chrono::DateTime<Utc>,
    pub can_unlink: bool,
}

impl From<OidcIdentityRecord> for OidcIdentityResponse {
    fn from(identity: OidcIdentityRecord) -> Self {
        Self {
            id: identity.id,
            issuer: identity.issuer,
            email_at_link: identity.email_at_link,
            last_login_at: identity.last_login_at,
            created_at: identity.created_at,
            can_unlink: identity.can_unlink,
        }
    }
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub struct OidcIdentityListResponse {
    pub data: Vec<OidcIdentityResponse>,
    pub linking_available: bool,
    pub has_local_password: bool,
}

#[utoipa::path(
    get,
    path = "/api/v1/oidc/identities",
    tag = "oidc",
    responses(
        (status = 200, description = "OIDC identities and authentication methods for the current account", body = OidcIdentityListResponse),
        (status = 401, description = "No active session", body = Problem)
    )
)]
pub(super) async fn list_identities(
    State(state): State<ApiState>,
    headers: HeaderMap,
) -> Result<Json<OidcIdentityListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    let store = require_store(&state)?;
    let identities = store
        .oidc_identities_for_user(principal.user_id)
        .await
        .map_err(map_oidc)?;
    let linking_available = store
        .oidc_configuration()
        .await
        .map_err(map_oidc)?
        .is_some_and(|configuration| configuration.enabled);
    let has_local_password = store
        .user_has_local_password(principal.user_id)
        .await
        .map_err(map_persistence)?
        .ok_or_else(|| Problem::unauthorized("The session is missing or expired."))?;
    Ok(Json(OidcIdentityListResponse {
        data: identities.into_iter().map(Into::into).collect(),
        linking_available,
        has_local_password,
    }))
}

#[utoipa::path(
    delete,
    path = "/api/v1/oidc/identities/{identity_id}",
    tag = "oidc",
    params(("identity_id" = Uuid, Path, description = "Linked identity ID")),
    responses(
        (status = 204, description = "Identity unlinked and session rotated"),
        (status = 401, description = "No active session", body = Problem),
        (status = 403, description = "CSRF or origin check failed", body = Problem),
        (status = 404, description = "Identity not linked to this account", body = Problem),
        (status = 409, description = "Unlink would remove the final authentication method", body = Problem),
        (status = 428, description = "Recent authentication is required", body = Problem)
    )
)]
pub(super) async fn unlink_identity(
    State(state): State<ApiState>,
    Path(identity_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    validate_session_cookie_ttl(state.session_ttl)?;
    let principal = require_mutation_session(&state, &headers).await?;
    let recent_auth_token = cookie(&headers, RECENT_AUTH_COOKIE)
        .filter(|value| value.len() == 43)
        .ok_or_else(reauthentication_required)?;
    let replacement_session = SessionMaterial::generate();
    require_store(&state)?
        .unlink_oidc_identity(UnlinkOidcIdentity {
            user_id: principal.user_id,
            identity_id,
            session_id: principal.session_id,
            security_version: principal.security_version,
            recent_auth_token_digest: RecentAuthMaterial::digest_token(recent_auth_token),
            replacement_session: &replacement_session,
            session_ttl: state.session_ttl,
        })
        .await
        .map_err(map_oidc)?;
    let mut response = StatusCode::NO_CONTENT.into_response();
    append_security_transition_cookies(&mut response, &replacement_session, state.session_ttl)?;
    Ok(response)
}
