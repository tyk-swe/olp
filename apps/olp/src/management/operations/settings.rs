use axum::{
    Json,
    extract::{Path, State, rejection::JsonRejection},
    http::HeaderMap,
    response::Response,
};
use chrono::{DateTime, Utc};
use olp_db::operations::settings::SettingRecord;
use olp_engine::domain::auth::Permission;
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;
use uuid::Uuid;

use super::helpers::{map_operations, not_found};
use crate::{
    bootstrap::mode_dependencies::ManagementState,
    management::{
        json_payload::json_payload,
        permissions::require_permission,
        preconditions::{if_match, with_etag},
        principal::{MutationPrincipal, ReadPrincipal},
        provenance::Provenance,
    },
    public_http::problem::Problem,
};

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(super) struct SettingResponse {
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
pub(super) struct SettingsResponse {
    data: Vec<SettingResponse>,
    items: Vec<SettingResponse>,
}

#[utoipa::path(
    get,
    path = "/api/v1/settings",
    tag = "settings",
    responses((status = 200, description = "Installation settings", body = SettingsResponse))
)]
pub(super) async fn list_settings(
    State(state): State<ManagementState>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<SettingsResponse>, Problem> {
    require_permission(&principal, Permission::ReadOperations)?;
    let settings = state.store().settings().await.map_err(map_operations)?;
    let items = settings.into_iter().map(Into::into).collect::<Vec<_>>();
    Ok(Json(SettingsResponse {
        data: items.clone(),
        items,
    }))
}

#[utoipa::path(
    get,
    path = "/api/v1/settings/{key}",
    tag = "settings",
    params(("key" = String, Path, description = "Setting key")),
    responses((status = 200, description = "Setting with ETag", body = SettingResponse))
)]
pub(super) async fn get_setting(
    State(state): State<ManagementState>,
    Path(key): Path<String>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ReadOperations)?;
    let setting = state
        .store()
        .settings()
        .await
        .map_err(map_operations)?
        .into_iter()
        .find(|setting| setting.key == key)
        .ok_or_else(not_found)?;
    setting_response(setting)
}

#[derive(Debug, Deserialize, ToSchema)]
pub(super) struct UpdateSettingRequest {
    value: String,
}

#[utoipa::path(
    put,
    path = "/api/v1/settings/{key}",
    tag = "settings",
    params(
        ("key" = String, Path, description = "Setting key"),
        ("If-Match" = String, Header, description = "Quoted setting ETag")
    ),
    request_body = UpdateSettingRequest,
    responses(
        (status = 200, description = "Updated setting", body = SettingResponse),
        (status = 412, description = "ETag mismatch", body = Problem)
    )
)]
pub(super) async fn update_setting(
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
    let setting = state
        .store()
        .with_provenance(&provenance)
        .update_setting(&key, &request.value, etag, principal.user_id)
        .await
        .map_err(map_operations)?;
    setting_response(setting)
}

fn setting_response(setting: SettingRecord) -> Result<Response, Problem> {
    let etag = setting.etag;
    with_etag(Json(SettingResponse::from(setting)), etag)
}
