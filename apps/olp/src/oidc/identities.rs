use axum::{
    Json,
    extract::{Path, State},
    http::{HeaderMap, StatusCode},
};
use chrono::Utc;
use olp_storage::OidcIdentityRecord;
use serde::Serialize;
use utoipa::ToSchema;
use uuid::Uuid;

use super::error::map_oidc;
use crate::{
    ApiState, Problem,
    management_api::{require_mutation_session, require_read_session, require_store},
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
}

#[utoipa::path(
    get,
    path = "/api/v1/oidc/identities",
    tag = "oidc",
    responses(
        (status = 200, description = "OIDC identities linked to the current account", body = OidcIdentityListResponse),
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
    Ok(Json(OidcIdentityListResponse {
        data: identities.into_iter().map(Into::into).collect(),
        linking_available,
    }))
}

#[utoipa::path(
    delete,
    path = "/api/v1/oidc/identities/{identity_id}",
    tag = "oidc",
    params(("identity_id" = Uuid, Path, description = "Linked identity ID")),
    responses(
        (status = 204, description = "Identity unlinked"),
        (status = 404, description = "Identity not linked to this account", body = Problem),
        (status = 409, description = "Unlink would remove the final authentication method", body = Problem)
    )
)]
pub(super) async fn unlink_identity(
    State(state): State<ApiState>,
    Path(identity_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<StatusCode, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_store(&state)?
        .unlink_oidc_identity(principal.user_id, identity_id)
        .await
        .map_err(map_oidc)?;
    Ok(StatusCode::NO_CONTENT)
}
