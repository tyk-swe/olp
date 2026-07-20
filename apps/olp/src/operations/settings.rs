use axum::{
    Json,
    extract::{Path, State, rejection::JsonRejection},
    http::{HeaderMap, HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
};
use chrono::{DateTime, Utc};
use olp_storage::SettingRecord;
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;
use uuid::Uuid;

use super::helpers::{map_operations, not_found};
use crate::{
    ApiState, Problem,
    management::{
        Permission, json_payload, require_mutation_session, require_permission,
        require_read_session, require_store,
    },
};

#[derive(Debug, Serialize, ToSchema)]
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
}

#[utoipa::path(
    get,
    path = "/api/v1/settings",
    tag = "settings",
    responses((status = 200, description = "Installation settings", body = SettingsResponse))
)]
pub(super) async fn list_settings(
    State(state): State<ApiState>,
    headers: HeaderMap,
) -> Result<Json<SettingsResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    let settings = require_store(&state)?
        .settings()
        .await
        .map_err(map_operations)?;
    Ok(Json(SettingsResponse {
        data: settings.into_iter().map(Into::into).collect(),
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
    State(state): State<ApiState>,
    headers: HeaderMap,
    Path(key): Path<String>,
) -> Result<Response, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    let setting = require_store(&state)?
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
    State(state): State<ApiState>,
    headers: HeaderMap,
    Path(key): Path<String>,
    payload: Result<Json<UpdateSettingRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageSettings)?;
    let etag = if_match(&headers)?;
    let request = json_payload(payload)?;
    let setting = require_store(&state)?
        .update_setting(&key, &request.value, etag, principal.user_id)
        .await
        .map_err(map_operations)?;
    setting_response(setting)
}

fn setting_response(setting: SettingRecord) -> Result<Response, Problem> {
    let etag =
        HeaderValue::from_str(&format!("\"{}\"", setting.etag)).map_err(|_| Problem::internal())?;
    let mut response = Json(SettingResponse::from(setting)).into_response();
    response.headers_mut().insert(header::ETAG, etag);
    Ok(response)
}

pub(super) fn if_match(headers: &HeaderMap) -> Result<Uuid, Problem> {
    let value = headers
        .get(header::IF_MATCH)
        .and_then(|value| value.to_str().ok())
        .ok_or_else(|| {
            Problem::new(
                StatusCode::PRECONDITION_REQUIRED,
                "if_match_required",
                "Precondition required",
                "An If-Match header containing the current ETag is required.",
            )
        })?;
    let value = value
        .strip_prefix('"')
        .and_then(|value| value.strip_suffix('"'))
        .ok_or_else(|| {
            Problem::bad_request("invalid_if_match", "If-Match must be a strong ETag.")
        })?;
    Uuid::parse_str(value)
        .map_err(|_| Problem::bad_request("invalid_if_match", "If-Match contains an invalid ETag."))
}
