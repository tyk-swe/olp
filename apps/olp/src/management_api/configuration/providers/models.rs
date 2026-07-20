use axum::{
    Json,
    extract::{Path, Query, State, rejection::JsonRejection},
    http::HeaderMap,
    response::Response,
};
use chrono::{DateTime, Utc};
use futures::{StreamExt as _, stream};
use olp_domain::ProviderKind;
use olp_providers::{
    CapabilityCertificationEvidence, CompatibleCapability, CompatibleCapabilityCertificationError,
    certifiable_capabilities,
};
use olp_storage::{
    CapabilityCertificationOutcome, CapabilityRecord, ConfigurationError, DiscoveredModelInput,
    ProviderModelInventoryRecord, ProviderModelRecord,
};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;
use uuid::Uuid;

use crate::{
    ApiState, Problem,
    management_api::{
        Permission, if_match, require_mutation_session, require_permission, require_read_session,
        require_store,
    },
};

use super::{ProviderDetailResponse, provider_connector};
use crate::management_api::configuration::common::{
    PageQuery, json, map_configuration_resource, page, validation, with_etag,
};

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ProviderCapabilityOptionsResponse {
    pub provider_kind: String,
    /// Capability tuples with a safe server-owned certification path for this
    /// provider kind. Configuration validation may support additional future tuples.
    pub capabilities: Vec<CapabilityInput>,
}

#[utoipa::path(
    get,
    path = "/api/v1/provider-kinds/{provider_kind}/capabilities",
    tag = "providers",
    params(("provider_kind" = String, Path, description = "Canonical provider kind")),
    responses(
        (status = 200, body = ProviderCapabilityOptionsResponse),
        (status = 400, body = Problem),
        (status = 401, body = Problem),
        (status = 403, body = Problem)
    )
)]
pub(crate) async fn list_provider_kind_capabilities(
    State(state): State<ApiState>,
    Path(provider_kind): Path<String>,
    headers: HeaderMap,
) -> Result<Json<ProviderCapabilityOptionsResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let provider_kind = provider_kind.parse::<ProviderKind>().map_err(|_| {
        Problem::bad_request(
            "invalid_provider_kind",
            "The provider kind is not supported by this installation.",
        )
    })?;

    Ok(Json(ProviderCapabilityOptionsResponse {
        provider_kind: provider_kind.as_str().to_owned(),
        capabilities: certifiable_capabilities(provider_kind)
            .map(|(operation, surface, mode)| CapabilityInput {
                operation: operation.as_str().to_owned(),
                surface: surface.as_str().to_owned(),
                mode: mode.as_str().to_owned(),
            })
            .collect(),
    }))
}

#[derive(Debug, Deserialize, ToSchema)]
pub(crate) struct ProviderModelInventoryQuery {
    pub cursor: Option<String>,
    pub limit: Option<u16>,
    pub enabled: Option<bool>,
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct CapabilityResponse {
    pub operation: String,
    pub surface: String,
    pub mode: String,
    pub source: String,
    pub certified_at: Option<DateTime<Utc>>,
}

impl From<CapabilityRecord> for CapabilityResponse {
    fn from(value: CapabilityRecord) -> Self {
        Self {
            operation: value.operation.to_string(),
            surface: value.surface.to_string(),
            mode: value.mode.to_string(),
            source: value.source.to_string(),
            certified_at: value.certified_at,
        }
    }
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ProviderModelResponse {
    pub id: Uuid,
    pub upstream_model: String,
    pub display_name: String,
    pub enabled: bool,
    pub discovered_at: Option<DateTime<Utc>>,
    pub capabilities: Vec<CapabilityResponse>,
}

impl From<ProviderModelRecord> for ProviderModelResponse {
    fn from(value: ProviderModelRecord) -> Self {
        Self {
            id: value.id,
            upstream_model: value.upstream_model,
            display_name: value.display_name,
            enabled: value.enabled,
            discovered_at: value.discovered_at,
            capabilities: value.capabilities.into_iter().map(Into::into).collect(),
        }
    }
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ProviderModelListResponse {
    pub items: Vec<ProviderModelResponse>,
    pub next_cursor: Option<String>,
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ProviderModelInventoryResponse {
    pub provider_id: Uuid,
    pub provider_name: String,
    pub provider_kind: String,
    pub model: ProviderModelResponse,
}

impl From<ProviderModelInventoryRecord> for ProviderModelInventoryResponse {
    fn from(value: ProviderModelInventoryRecord) -> Self {
        Self {
            provider_id: value.provider_id,
            provider_name: value.provider_name,
            provider_kind: value.provider_kind.to_string(),
            model: value.model.into(),
        }
    }
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ProviderModelInventoryListResponse {
    pub items: Vec<ProviderModelInventoryResponse>,
    pub next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/provider-models",
    tag = "providers",
    params(
        ("cursor" = Option<String>, Query),
        ("limit" = Option<u16>, Query, minimum = 1, maximum = 100),
        ("enabled" = Option<bool>, Query, description = "Optional enabled-state filter")
    ),
    responses(
        (status = 200, description = "Bounded cross-provider model and capability page", body = ProviderModelInventoryListResponse)
    )
)]
pub(crate) async fn list_provider_model_inventory(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<ProviderModelInventoryQuery>,
) -> Result<Json<ProviderModelInventoryListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let enabled = query.enabled;
    let (cursor, limit) = page(PageQuery {
        cursor: query.cursor,
        limit: query.limit,
    })?;
    let page = require_store(&state)?
        .list_provider_model_inventory(cursor, limit, enabled)
        .await
        .map_err(map_configuration_resource)?;
    Ok(Json(ProviderModelInventoryListResponse {
        items: page.items.into_iter().map(Into::into).collect(),
        next_cursor: page.next_cursor.map(|value| value.to_string()),
    }))
}

#[utoipa::path(
    get,
    path = "/api/v1/providers/{provider_id}/models",
    tag = "providers",
    params(
        ("provider_id" = Uuid, Path),
        ("cursor" = Option<String>, Query),
        ("limit" = Option<u16>, Query, minimum = 1, maximum = 100)
    ),
    responses(
        (status = 200, description = "Bounded provider model and capability page", body = ProviderModelListResponse),
        (status = 404, body = Problem)
    )
)]
pub(crate) async fn list_provider_models(
    State(state): State<ApiState>,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<ProviderModelListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let page = require_store(&state)?
        .list_provider_models(provider_id, cursor, limit)
        .await
        .map_err(map_configuration_resource)?;
    Ok(Json(ProviderModelListResponse {
        items: page.items.into_iter().map(Into::into).collect(),
        next_cursor: page.next_cursor.map(|value| value.to_string()),
    }))
}

#[derive(Clone, Debug, Deserialize, Serialize, ToSchema)]
pub(crate) struct CapabilityInput {
    pub operation: String,
    pub surface: String,
    pub mode: String,
}

fn capability_record(input: CapabilityInput) -> Result<CapabilityRecord, Problem> {
    Ok(CapabilityRecord {
        operation: input
            .operation
            .parse()
            .map_err(|_| validation("capabilities", "A reviewed operation is invalid."))?,
        surface: input
            .surface
            .parse()
            .map_err(|_| validation("capabilities", "A reviewed surface is invalid."))?,
        mode: input
            .mode
            .parse()
            .map_err(|_| validation("capabilities", "A reviewed mode is invalid."))?,
        source: olp_domain::CapabilitySource::Declared,
        certified_at: None,
    })
}

#[derive(Clone, Debug, Deserialize, ToSchema)]
pub(crate) struct DiscoveredModelRequest {
    pub upstream_model: String,
    pub display_name: String,
}

#[derive(Debug, Deserialize, ToSchema)]
pub(crate) struct DiscoverModelsRequest {
    /// Omit or pass an empty array to query the upstream model-list API.
    /// Manual identifiers are a fallback for upstreams without a list API.
    /// All discovered models start disabled and without capability claims until
    /// the explicit review operation is completed.
    #[serde(default)]
    pub models: Vec<DiscoveredModelRequest>,
}

#[utoipa::path(
    post,
    path = "/api/v1/providers/{provider_id}/discovery",
    tag = "providers",
    params(("provider_id" = Uuid, Path), ("If-Match" = String, Header)),
    request_body = DiscoverModelsRequest,
    responses((status = 200, body = ProviderDetailResponse), (status = 412, body = Problem), (status = 422, body = Problem))
)]
pub(crate) async fn discover_provider_models(
    State(state): State<ApiState>,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
    payload: Result<Json<DiscoverModelsRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageProviders)?;
    let request = json(payload)?;
    let models: Vec<DiscoveredModelInput> = if request.models.is_empty() {
        provider_connector(&state, provider_id)
            .await?
            .discover_models()
            .await
            .map_err(|detail| validation("provider", &detail))?
            .into_iter()
            .map(|model| DiscoveredModelInput {
                upstream_model: model.id,
                display_name: model.display_name,
                enabled: false,
                capabilities: Vec::new(),
            })
            .collect()
    } else {
        request
            .models
            .into_iter()
            .map(|model| DiscoveredModelInput {
                upstream_model: model.upstream_model,
                display_name: model.display_name,
                enabled: false,
                capabilities: Vec::new(),
            })
            .collect()
    };
    let store = require_store(&state)?;
    let etag = store
        .discover_provider_models(provider_id, if_match(&headers)?, &models, principal.user_id)
        .await
        .map_err(map_configuration_resource)?;
    let provider: ProviderDetailResponse = store
        .get_provider(provider_id)
        .await
        .map_err(map_configuration_resource)?
        .into();
    with_etag(Json(provider), etag)
}

#[derive(Debug, Deserialize, ToSchema)]
pub(crate) struct SetModelRequest {
    pub enabled: bool,
    /// Explicit operator-reviewed capability tuples. Their provenance is
    /// recorded as `declared`; certification/probe jobs may promote provenance
    /// separately and cannot be forged by the browser.
    #[serde(default)]
    pub capabilities: Vec<CapabilityInput>,
}

#[utoipa::path(
    patch,
    path = "/api/v1/providers/{provider_id}/models/{model_id}",
    tag = "providers",
    params(("provider_id" = Uuid, Path), ("model_id" = Uuid, Path), ("If-Match" = String, Header)),
    request_body = SetModelRequest,
    responses((status = 200, body = ProviderDetailResponse), (status = 412, body = Problem))
)]
pub(crate) async fn set_provider_model(
    State(state): State<ApiState>,
    Path((provider_id, model_id)): Path<(Uuid, Uuid)>,
    headers: HeaderMap,
    payload: Result<Json<SetModelRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageProviders)?;
    let request = json(payload)?;
    let store = require_store(&state)?;
    let etag = store
        .set_provider_model_enabled(
            provider_id,
            model_id,
            request.enabled,
            &request
                .capabilities
                .into_iter()
                .map(capability_record)
                .collect::<Result<Vec<_>, _>>()?,
            if_match(&headers)?,
            principal.user_id,
        )
        .await
        .map_err(map_configuration_resource)?;
    let provider: ProviderDetailResponse = store
        .get_provider(provider_id)
        .await
        .map_err(map_configuration_resource)?
        .into();
    with_etag(Json(provider), etag)
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct CapabilityCertificationItemResponse {
    pub operation: String,
    pub surface: String,
    pub mode: String,
    pub succeeded: bool,
    pub error_code: Option<String>,
    pub detail: String,
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct CapabilityCertificationResponse {
    pub provider_id: Uuid,
    pub model_id: Uuid,
    pub status: String,
    pub checked_at: DateTime<Utc>,
    pub certified_count: usize,
    pub attempted_count: usize,
    pub results: Vec<CapabilityCertificationItemResponse>,
}

#[utoipa::path(
    post,
    path = "/api/v1/providers/{provider_id}/models/{model_id}/certify",
    tag = "providers",
    params(
        ("provider_id" = Uuid, Path),
        ("model_id" = Uuid, Path),
        ("If-Match" = String, Header, description = "Current provider ETag")
    ),
    responses(
        (status = 200, description = "Provider/model capability certification completed", body = CapabilityCertificationResponse),
        (status = 409, description = "Provider is active", body = Problem),
        (status = 412, description = "Provider or reviewed capabilities changed", body = Problem),
        (status = 422, description = "Provider or capability set cannot be certified", body = Problem)
    )
)]
pub(crate) async fn certify_provider_model(
    State(state): State<ApiState>,
    Path((provider_id, model_id)): Path<(Uuid, Uuid)>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageProviders)?;
    let expected_etag = if_match(&headers)?;
    let store = require_store(&state)?;
    let provider = store
        .get_provider(provider_id)
        .await
        .map_err(map_configuration_resource)?;
    if provider.etag != expected_etag {
        return Err(map_configuration_resource(
            ConfigurationError::PreconditionFailed,
        ));
    }
    if provider.state != olp_domain::ProviderState::Draft {
        return Err(map_configuration_resource(ConfigurationError::InUse));
    }
    let model = store
        .get_provider_model(provider_id, model_id)
        .await
        .map_err(map_configuration_resource)?;
    if model.capabilities.is_empty() || model.capabilities.len() > 16 {
        return Err(validation(
            "capabilities",
            "Review between 1 and 16 capability tuples before certification.",
        ));
    }
    let upstream_model = model.upstream_model;
    let connector = provider_connector(&state, provider_id).await?;
    let results = stream::iter(model.capabilities.into_iter().map(|capability| {
        let connector = &connector;
        let upstream_model = &upstream_model;
        async move {
            let tuple = compatible_capability(&capability)?;
            let result = connector.certify_capability(upstream_model, tuple).await;
            Ok::<_, Problem>(certification_item(capability, result))
        }
    }))
    .buffered(4)
    .collect::<Vec<_>>()
    .await
    .into_iter()
    .collect::<Result<Vec<_>, _>>()?;

    let outcomes = results
        .iter()
        .map(|result| {
            Ok::<_, Problem>(CapabilityCertificationOutcome {
                operation: result.operation.parse().map_err(|_| Problem::internal())?,
                surface: result.surface.parse().map_err(|_| Problem::internal())?,
                mode: result.mode.parse().map_err(|_| Problem::internal())?,
                succeeded: result.succeeded,
            })
        })
        .collect::<Result<Vec<_>, _>>()?;
    let applied = store
        .apply_compatible_capability_certification(
            provider_id,
            model_id,
            expected_etag,
            principal.user_id,
            &outcomes,
        )
        .await
        .map_err(map_configuration_resource)?;
    let status = if applied.certified_count == applied.attempted_count {
        "succeeded"
    } else if applied.certified_count == 0 {
        "failed"
    } else {
        "partial"
    };
    with_etag(
        Json(CapabilityCertificationResponse {
            provider_id,
            model_id,
            status: status.to_owned(),
            checked_at: applied.certified_at,
            certified_count: applied.certified_count,
            attempted_count: applied.attempted_count,
            results,
        }),
        applied.etag,
    )
}

fn compatible_capability(capability: &CapabilityRecord) -> Result<CompatibleCapability, Problem> {
    Ok(CompatibleCapability {
        operation: capability.operation,
        surface: capability.surface,
        mode: capability.mode,
    })
}

pub(crate) fn certification_item(
    capability: CapabilityRecord,
    result: Result<CapabilityCertificationEvidence, CompatibleCapabilityCertificationError>,
) -> CapabilityCertificationItemResponse {
    let (succeeded, error_code, detail) = match result {
        Ok(CapabilityCertificationEvidence::LiveProbe) => (
            true,
            None,
            "The endpoint completed the bounded request and passed the production response codec."
                .to_owned(),
        ),
        Ok(CapabilityCertificationEvidence::NativeOpenAiModelDiscoveryAndConnectorContract) => (
            true,
            None,
            "The official OpenAI endpoint returned the exact provider model from credentialed bounded discovery, and this tuple is in the closed native connector contract."
                .to_owned(),
        ),
        Err(CompatibleCapabilityCertificationError::Unsupported) => (
            false,
            Some("unsafe_or_unsupported_probe".to_owned()),
            "This tuple has no safe bounded live probe and was not certified.".to_owned(),
        ),
        Err(CompatibleCapabilityCertificationError::Transport { phase, class }) => (
            false,
            Some(transport_failure_code(class).to_owned()),
            format!("The live endpoint probe failed during {phase:?}."),
        ),
        Err(CompatibleCapabilityCertificationError::InvalidResult) => (
            false,
            Some("invalid_probe_result".to_owned()),
            "The live endpoint response did not prove the requested capability.".to_owned(),
        ),
        Err(CompatibleCapabilityCertificationError::ModelNotDiscovered) => (
            false,
            Some("model_not_discovered".to_owned()),
            "Credentialed model discovery did not return the exact reviewed provider model."
                .to_owned(),
        ),
    };
    CapabilityCertificationItemResponse {
        operation: capability.operation.to_string(),
        surface: capability.surface.to_string(),
        mode: capability.mode.to_string(),
        succeeded,
        error_code,
        detail,
    }
}

const fn transport_failure_code(class: olp_domain::AttemptFailureClass) -> &'static str {
    match class {
        olp_domain::AttemptFailureClass::Connect => "connect_failed",
        olp_domain::AttemptFailureClass::Timeout => "timeout",
        olp_domain::AttemptFailureClass::RateLimit => "rate_limited",
        olp_domain::AttemptFailureClass::UpstreamServer => "upstream_server_error",
        olp_domain::AttemptFailureClass::UpstreamClient => "upstream_rejected_probe",
        olp_domain::AttemptFailureClass::Protocol => "protocol_mismatch",
        olp_domain::AttemptFailureClass::Cancelled => "cancelled",
        olp_domain::AttemptFailureClass::Ambiguous => "ambiguous_result",
    }
}
