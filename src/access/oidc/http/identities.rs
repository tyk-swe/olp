use crate::access::oidc::repository::types::OidcIdentityRecord;
use crate::access::oidc::repository::types::UnlinkOidcIdentity;
use crate::crypto::session_material::RecentAuthMaterial;
use crate::crypto::session_material::SessionMaterial;
use axum::Json;
use axum::extract::Path;
use axum::extract::State;
use axum::http::HeaderMap;
use axum::http::StatusCode;
use axum::response::IntoResponse;
use axum::response::Response;
use chrono::Utc;
use serde::Serialize;
use utoipa::ToSchema;
use uuid::Uuid;

use crate::access::cookies::append_security_transition_cookies;
use crate::access::cookies::validate_session_cookie_ttl;
use crate::access::oidc::http::error::map_oidc;
use crate::access::principal::MutationPrincipal;
use crate::access::principal::ReadPrincipal;
use crate::access::sessions::cookie;
use crate::access::sessions::reauthentication_required;
use crate::http::control::error_mapping::map_persistence;
use crate::http::control::provenance::Provenance;
use crate::http::control::state::ManagementState;
use crate::http::problem::Problem;
use crate::http::request_cookies::RECENT_AUTH_COOKIE;

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct OidcIdentityResponse {
    pub(crate) id: Uuid,
    pub(crate) issuer: String,
    pub(crate) email_at_link: Option<String>,
    pub(crate) last_login_at: Option<chrono::DateTime<Utc>>,
    pub(crate) created_at: chrono::DateTime<Utc>,
    pub(crate) can_unlink: bool,
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
pub(crate) struct OidcIdentityListResponse {
    pub(crate) data: Vec<OidcIdentityResponse>,
    pub(crate) items: Vec<OidcIdentityResponse>,
    pub(crate) linking_available: bool,
    pub(crate) has_local_password: bool,
}

#[utoipa::path(
    get,
    path = "/api/v3/oidc/identities",
    tag = "oidc",
    responses(
        (status = 200, description = "OIDC identities and authentication methods for the current account", body = OidcIdentityListResponse),
        (status = 401, description = "No active session", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [])))]
pub(crate) async fn list_identities(
    State(state): State<ManagementState>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<OidcIdentityListResponse>, Problem> {
    let pool = &state.request_boundary.pool;
    let identities = crate::access::oidc::repository::identities::oidc_identities_for_user(
        pool,
        principal.user_id,
    )
    .await
    .map_err(map_oidc)?;
    let linking_available =
        crate::access::oidc::repository::configuration::oidc_configuration(pool)
            .await
            .map_err(map_oidc)?
            .is_some_and(|configuration| configuration.enabled);
    let has_local_password =
        crate::access::authentication::user_has_local_password(pool, principal.user_id)
            .await
            .map_err(map_persistence)?
            .ok_or_else(|| Problem::unauthorized("The session is missing or expired."))?;
    let items = identities.into_iter().map(Into::into).collect::<Vec<_>>();
    Ok(Json(OidcIdentityListResponse {
        data: items.clone(),
        items,
        linking_available,
        has_local_password,
    }))
}

#[utoipa::path(
    delete,
    path = "/api/v3/oidc/identities/{identity_id}",
    tag = "oidc",
    params(("identity_id" = Uuid, Path, description = "Linked identity ID")),
    responses(
        (status = 204, description = "Identity unlinked and session rotated"),
        (status = 401, description = "No active session", body = Problem, content_type = "application/problem+json"),
        (status = 403, description = "CSRF or origin check failed", body = Problem, content_type = "application/problem+json"),
        (status = 404, description = "Identity not linked to this account", body = Problem, content_type = "application/problem+json"),
        (status = 409, description = "Unlink would remove the final authentication method", body = Problem, content_type = "application/problem+json"),
        (status = 428, description = "Recent authentication is required", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [], "csrfToken" = [])))]
pub(crate) async fn unlink_identity(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    Path(identity_id): Path<Uuid>,
    headers: HeaderMap,
    MutationPrincipal(principal): MutationPrincipal,
) -> Result<Response, Problem> {
    validate_session_cookie_ttl(state.session_ttl)?;
    let recent_auth_token = cookie(&headers, RECENT_AUTH_COOKIE)?
        .filter(|value| value.len() == 43)
        .ok_or_else(reauthentication_required)?;
    let replacement_session = SessionMaterial::generate();
    crate::access::oidc::repository::identities::unlink_oidc_identity(
        &state.request_boundary.pool,
        &provenance,
        UnlinkOidcIdentity {
            user_id: principal.user_id,
            identity_id,
            session_id: principal.session_id,
            security_version: principal.security_version,
            recent_auth_token_digest: RecentAuthMaterial::digest_token(recent_auth_token),
            replacement_session: &replacement_session,
            session_ttl: state.session_ttl,
        },
    )
    .await
    .map_err(map_oidc)?;
    let mut response = StatusCode::NO_CONTENT.into_response();
    append_security_transition_cookies(&mut response, &replacement_session, state.session_ttl)?;
    Ok(response)
}
