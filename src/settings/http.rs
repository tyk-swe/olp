use crate::access::policy::Permission;
use crate::settings::repository::SettingRecord;
use axum::Json;
use axum::extract::Path;
use axum::extract::State;
use axum::extract::rejection::JsonRejection;
use axum::http::HeaderMap;
use axum::response::Response;
use chrono::DateTime;
use chrono::Utc;
use serde::Deserialize;
use serde::Serialize;
use utoipa::ToSchema;
use uuid::Uuid;

use crate::access::permissions::require_permission;
use crate::access::principal::MutationPrincipal;
use crate::access::principal::ReadPrincipal;
use crate::http::control::json_payload::json_payload;
use crate::http::control::operations::helpers::map_operations;
use crate::http::control::operations::helpers::not_found;
use crate::http::control::preconditions::if_match;
use crate::http::control::preconditions::with_etag;
use crate::http::control::provenance::Provenance;
use crate::http::control::state::ManagementState;
use crate::http::problem::Problem;

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct SettingResponse {
    key: String,
    value: String,
    #[schema(value_type = String, format = Uuid)]
    etag: Uuid,
    #[schema(value_type = String, format = Uuid)]
    updated_by: Uuid,
    updated_at: DateTime<Utc>,
}

impl From<SettingRecord> for SettingResponse {
    fn from(record: SettingRecord) -> Self {
        Self {
            key: record.key,
            value: record.value,
            etag: record.etag,
            updated_by: record.updated_by,
            updated_at: record.updated_at,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct SettingsResponse {
    data: Vec<SettingResponse>,
    items: Vec<SettingResponse>,
}

#[utoipa::path(
    get,
    path = "/api/v3/settings",
    tag = "settings",
    responses((status = 200, description = "Installation settings", body = SettingsResponse))
, security(("sessionCookie" = [])))]
pub(crate) async fn list_settings(
    State(state): State<ManagementState>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<SettingsResponse>, Problem> {
    require_permission(&principal, Permission::ReadOperations)?;
    let settings = crate::settings::repository::settings(&state.request_boundary.pool)
        .await
        .map_err(map_operations)?;
    let items = settings.into_iter().map(Into::into).collect::<Vec<_>>();
    Ok(Json(SettingsResponse {
        data: items.clone(),
        items,
    }))
}

#[utoipa::path(
    get,
    path = "/api/v3/settings/{key}",
    tag = "settings",
    params(("key" = String, Path, description = "Setting key")),
    responses((status = 200, description = "Setting with ETag", body = SettingResponse))
, security(("sessionCookie" = [])))]
pub(crate) async fn get_setting(
    State(state): State<ManagementState>,
    Path(key): Path<String>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ReadOperations)?;
    let setting = crate::settings::repository::settings(&state.request_boundary.pool)
        .await
        .map_err(map_operations)?
        .into_iter()
        .find(|setting| setting.key == key)
        .ok_or_else(not_found)?;
    setting_response(setting)
}

#[derive(Debug, Deserialize, ToSchema)]
pub(crate) struct UpdateSettingRequest {
    value: String,
}

#[utoipa::path(
    put,
    path = "/api/v3/settings/{key}",
    tag = "settings",
    params(
        ("key" = String, Path, description = "Setting key"),
        ("If-Match" = String, Header, description = "Quoted setting ETag")
    ),
    request_body = UpdateSettingRequest,
    responses(
        (status = 200, description = "Updated setting", body = SettingResponse),
        (status = 412, description = "ETag mismatch", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [], "csrfToken" = [])))]
pub(crate) async fn update_setting(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    headers: HeaderMap,
    Path(key): Path<String>,
    MutationPrincipal(principal): MutationPrincipal,
    payload: Result<Json<UpdateSettingRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ManageSettings)?;
    let etag = if_match(&headers)?;
    let request = json_payload(payload)?;
    let setting = crate::settings::repository::update_setting(
        &state.request_boundary.pool,
        &provenance,
        &key,
        &request.value,
        etag,
        principal.user_id,
    )
    .await
    .map_err(map_operations)?;
    setting_response(setting)
}

fn setting_response(setting: SettingRecord) -> Result<Response, Problem> {
    let etag = setting.etag;
    with_etag(Json(SettingResponse::from(setting)), etag)
}

pub(crate) fn router()
-> utoipa_axum::router::OpenApiRouter<crate::http::control::state::ManagementState> {
    utoipa_axum::router::OpenApiRouter::new()
        .routes(utoipa_axum::routes!(crate::settings::http::get_setting))
        .routes(utoipa_axum::routes!(crate::settings::http::list_settings))
        .routes(utoipa_axum::routes!(crate::settings::http::update_setting))
}
