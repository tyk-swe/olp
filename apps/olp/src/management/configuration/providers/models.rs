use axum::{
    Json,
    extract::{Path, Query, State, rejection::JsonRejection},
    http::HeaderMap,
    response::Response,
};
use chrono::{DateTime, Utc};
use futures::{StreamExt as _, stream};
use olp_db::{
    configuration::Error, configuration::resources::CapabilityCertificationOutcome,
    configuration::resources::CapabilityRecord, configuration::resources::DiscoveredModelInput,
    configuration::resources::ProviderModelInventoryRecord,
    configuration::resources::ProviderModelRecord,
};
use olp_engine::domain::{
    auth::Permission,
    provider::ProviderAuthMode,
    provider_configuration::{
        CredentialRequirement, Field, ProviderKindSpec, ProviderPresetSpec, provider_kind_specs,
    },
    routing::provider::ProviderKind,
};
use olp_engine::providers::{
    factory::certification::{CapabilityCertificationEvidence, certifiable_capabilities},
    openai::certification::{CompatibleCapability, CompatibleCapabilityCertificationError},
};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;
use uuid::Uuid;

use crate::{
    bootstrap::mode_dependencies::ManagementState,
    management::{
        error_mapping::map_configuration,
        json_payload::json_payload,
        pagination::{PageQuery, page},
        permissions::require_permission,
        preconditions::{if_match, with_etag},
        sessions::{require_mutation_session, require_read_session},
    },
    public_http::problem::Problem,
};

use super::manage::{ProviderDetailResponse, load_provider_detail};
use crate::bootstrap::provider_adapter::provider_connector;

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ProviderCapabilityOptionsResponse {
    pub provider_kind: ProviderKind,
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
    State(state): State<ManagementState>,
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
        provider_kind,
        capabilities: certifiable_capabilities(provider_kind)
            .map(|(operation, surface, mode)| CapabilityInput {
                operation: operation.as_str().to_owned(),
                surface: surface.as_str().to_owned(),
                mode: mode.as_str().to_owned(),
            })
            .collect(),
    }))
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ProviderAuthCapabilityResponse {
    pub mode: ProviderAuthMode,
    pub label: String,
    pub credential: CredentialRequirement,
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ProviderFieldCapabilityResponse {
    pub field: Field,
    pub label: String,
    pub required: bool,
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ProviderPresetResponse {
    /// Stable identifier for this immutable catalog entry.
    pub id: String,
    pub label: String,
    pub description: String,
    /// Reviewed HTTPS base URL resolved into ordinary provider configuration.
    pub endpoint: String,
    pub auth_mode: ProviderAuthMode,
    /// Organization maintaining the official documentation used for review.
    pub maintainer: String,
    pub documentation_label: String,
    pub documentation_url: String,
}

impl From<&ProviderPresetSpec> for ProviderPresetResponse {
    fn from(spec: &ProviderPresetSpec) -> Self {
        Self {
            id: spec.id.to_owned(),
            label: spec.label.to_owned(),
            description: spec.description.to_owned(),
            endpoint: spec.endpoint.to_owned(),
            auth_mode: spec.auth_mode,
            maintainer: spec.maintainer.to_owned(),
            documentation_label: spec.documentation_label.to_owned(),
            documentation_url: spec.documentation_url.to_owned(),
        }
    }
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ProviderKindCapabilityResponse {
    pub kind: ProviderKind,
    pub label: String,
    pub description: String,
    pub default_auth_mode: ProviderAuthMode,
    pub auth_modes: Vec<ProviderAuthCapabilityResponse>,
    pub fields: Vec<ProviderFieldCapabilityResponse>,
    /// Reviewed onboarding presets. Empty for provider kinds without presets.
    pub presets: Vec<ProviderPresetResponse>,
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ProviderKindCapabilityListResponse {
    pub items: Vec<ProviderKindCapabilityResponse>,
}

#[utoipa::path(
    get,
    path = "/api/v1/provider-kinds",
    tag = "providers",
    responses(
        (status = 200, body = ProviderKindCapabilityListResponse),
        (status = 401, body = Problem),
        (status = 403, body = Problem)
    )
)]
pub(crate) async fn list_provider_kinds(
    State(state): State<ManagementState>,
    headers: HeaderMap,
) -> Result<Json<ProviderKindCapabilityListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let items = provider_kind_specs()
        .iter()
        .map(provider_kind_capability_response)
        .collect();
    Ok(Json(ProviderKindCapabilityListResponse { items }))
}

fn provider_kind_capability_response(spec: &ProviderKindSpec) -> ProviderKindCapabilityResponse {
    ProviderKindCapabilityResponse {
        kind: spec.kind,
        label: spec.label.to_owned(),
        description: spec.description.to_owned(),
        default_auth_mode: spec.default_auth_mode,
        auth_modes: spec
            .auth_modes
            .iter()
            .map(|auth| ProviderAuthCapabilityResponse {
                mode: auth.mode,
                label: auth.label.to_owned(),
                credential: auth.credential,
            })
            .collect(),
        fields: spec
            .fields
            .iter()
            .map(|field| ProviderFieldCapabilityResponse {
                field: field.field,
                label: field.label.to_owned(),
                required: field.required,
            })
            .collect(),
        presets: spec
            .presets
            .iter()
            .map(ProviderPresetResponse::from)
            .collect(),
    }
}

#[cfg(test)]
mod provider_kind_catalog_tests {
    use super::*;

    #[test]
    fn management_metadata_exposes_presets_only_on_the_compatible_kind() {
        let responses: Vec<_> = provider_kind_specs()
            .iter()
            .map(provider_kind_capability_response)
            .collect();
        let compatible = responses
            .iter()
            .find(|response| response.kind == ProviderKind::OpenAiCompatible)
            .expect("compatible provider kind must be present");
        assert_eq!(
            compatible
                .presets
                .iter()
                .map(|preset| preset.id.as_str())
                .collect::<Vec<_>>(),
            [
                "groq",
                "mistral_ai",
                "together_ai",
                "xai",
                "cerebras",
                "openrouter"
            ]
        );
        assert!(compatible.presets.iter().all(|preset| {
            preset.endpoint.starts_with("https://")
                && preset.documentation_url.starts_with("https://")
                && preset.auth_mode == ProviderAuthMode::ApiKey
        }));
        assert!(
            responses
                .iter()
                .filter(|response| response.kind != ProviderKind::OpenAiCompatible)
                .all(|response| response.presets.is_empty())
        );
    }
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
    pub provider_kind: ProviderKind,
    pub model: ProviderModelResponse,
}

impl From<ProviderModelInventoryRecord> for ProviderModelInventoryResponse {
    fn from(value: ProviderModelInventoryRecord) -> Self {
        Self {
            provider_id: value.provider_id,
            provider_name: value.provider_name,
            provider_kind: value.provider_kind,
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
    State(state): State<ManagementState>,
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
    let page = state
        .store()
        .list_provider_model_inventory(cursor, limit, enabled)
        .await
        .map_err(map_configuration)?;
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
    State(state): State<ManagementState>,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<ProviderModelListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let page = state
        .store()
        .list_provider_models(provider_id, cursor, limit)
        .await
        .map_err(map_configuration)?;
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
        operation: input.operation.parse().map_err(|_| {
            Problem::field_validation("capabilities", "A reviewed operation is invalid.")
        })?,
        surface: input.surface.parse().map_err(|_| {
            Problem::field_validation("capabilities", "A reviewed surface is invalid.")
        })?,
        mode: input.mode.parse().map_err(|_| {
            Problem::field_validation("capabilities", "A reviewed mode is invalid.")
        })?,
        source: olp_engine::domain::provider::CapabilitySource::Declared,
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
    State(state): State<ManagementState>,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
    payload: Result<Json<DiscoverModelsRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageProviders)?;
    let request = json_payload(payload)?;
    let models: Vec<DiscoveredModelInput> = if request.models.is_empty() {
        provider_connector(&state, provider_id)
            .await?
            .discover_models()
            .await
            .map_err(|detail| Problem::field_validation("provider", detail))?
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
    let store = state.store();
    let etag = store
        .discover_provider_models(provider_id, if_match(&headers)?, &models, principal.user_id)
        .await
        .map_err(map_configuration)?;
    let provider = load_provider_detail(store, provider_id).await?;
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
    State(state): State<ManagementState>,
    Path((provider_id, model_id)): Path<(Uuid, Uuid)>,
    headers: HeaderMap,
    payload: Result<Json<SetModelRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageProviders)?;
    let request = json_payload(payload)?;
    let store = state.store();
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
        .map_err(map_configuration)?;
    let provider = load_provider_detail(store, provider_id).await?;
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
    State(state): State<ManagementState>,
    Path((provider_id, model_id)): Path<(Uuid, Uuid)>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageProviders)?;
    let expected_etag = if_match(&headers)?;
    let store = state.store();
    let provider = store
        .get_provider(provider_id)
        .await
        .map_err(map_configuration)?;
    if provider.etag != expected_etag {
        return Err(map_configuration(Error::PreconditionFailed));
    }
    if provider.state != olp_engine::domain::provider::ProviderState::Draft {
        return Err(map_configuration(Error::InUse));
    }
    let model = store
        .get_provider_model(provider_id, model_id)
        .await
        .map_err(map_configuration)?;
    if model.capabilities.is_empty() || model.capabilities.len() > 16 {
        return Err(Problem::field_validation(
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
        .map_err(map_configuration)?;
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

const fn transport_failure_code(
    class: olp_engine::domain::ports::AttemptFailureClass,
) -> &'static str {
    match class {
        olp_engine::domain::ports::AttemptFailureClass::Connect => "connect_failed",
        olp_engine::domain::ports::AttemptFailureClass::Timeout => "timeout",
        olp_engine::domain::ports::AttemptFailureClass::RateLimit => "rate_limited",
        olp_engine::domain::ports::AttemptFailureClass::UpstreamServer => "upstream_server_error",
        olp_engine::domain::ports::AttemptFailureClass::UpstreamClient => "upstream_rejected_probe",
        olp_engine::domain::ports::AttemptFailureClass::Protocol => "protocol_mismatch",
        olp_engine::domain::ports::AttemptFailureClass::Cancelled => "cancelled",
        olp_engine::domain::ports::AttemptFailureClass::Ambiguous => "ambiguous_result",
    }
}

#[cfg(test)]
mod model_tests {
    use chrono::Utc;
    use olp_engine::domain::{
        canonical::identity::{OperationKind, Surface, TransportMode},
        ports::{AttemptFailureClass, TransportPhase},
        provider::CapabilitySource,
    };

    use super::*;

    fn capability() -> CapabilityRecord {
        CapabilityRecord {
            operation: OperationKind::Generation,
            surface: Surface::OpenAi,
            mode: TransportMode::Unary,
            source: CapabilitySource::Certified,
            certified_at: Some(Utc::now()),
        }
    }

    #[test]
    fn model_inventory_conversion_preserves_nested_capability_metadata() {
        let provider_id = Uuid::from_u128(1);
        let model_id = Uuid::from_u128(2);
        let capability_record = capability();
        let certified_at = capability_record.certified_at;
        let response = ProviderModelInventoryResponse::from(ProviderModelInventoryRecord {
            provider_id,
            provider_name: "production".to_owned(),
            provider_kind: ProviderKind::OpenAiCompatible,
            model: ProviderModelRecord {
                id: model_id,
                upstream_model: "upstream-model".to_owned(),
                display_name: "Display model".to_owned(),
                enabled: true,
                discovered_at: None,
                capabilities: vec![capability_record],
            },
        });

        assert_eq!(response.provider_id, provider_id);
        assert_eq!(response.provider_name, "production");
        assert_eq!(response.provider_kind, ProviderKind::OpenAiCompatible);
        assert_eq!(response.model.id, model_id);
        assert_eq!(response.model.upstream_model, "upstream-model");
        assert_eq!(response.model.display_name, "Display model");
        assert!(response.model.enabled);
        assert_eq!(response.model.capabilities.len(), 1);
        let capability = &response.model.capabilities[0];
        assert_eq!(capability.operation, "generation");
        assert_eq!(capability.surface, "openai");
        assert_eq!(capability.mode, "unary");
        assert_eq!(capability.source, "certified");
        assert_eq!(capability.certified_at, certified_at);
    }

    #[test]
    fn capability_input_parses_closed_sets_and_identifies_the_bad_dimension() {
        let input = |operation: &str, surface: &str, mode: &str| CapabilityInput {
            operation: operation.to_owned(),
            surface: surface.to_owned(),
            mode: mode.to_owned(),
        };
        let parsed = capability_record(input("generation", "openai", "unary")).unwrap();
        assert_eq!(parsed.operation, OperationKind::Generation);
        assert_eq!(parsed.surface, Surface::OpenAi);
        assert_eq!(parsed.mode, TransportMode::Unary);
        assert_eq!(parsed.source, CapabilitySource::Declared);
        assert!(parsed.certified_at.is_none());
        let compatible = compatible_capability(&parsed).unwrap();
        assert_eq!(compatible.operation, parsed.operation);
        assert_eq!(compatible.surface, parsed.surface);
        assert_eq!(compatible.mode, parsed.mode);

        for (invalid, detail) in [
            (
                input("unknown", "openai", "unary"),
                "A reviewed operation is invalid.",
            ),
            (
                input("generation", "unknown", "unary"),
                "A reviewed surface is invalid.",
            ),
            (
                input("generation", "openai", "unknown"),
                "A reviewed mode is invalid.",
            ),
        ] {
            let problem = capability_record(invalid).unwrap_err();
            assert_eq!(problem.status, 422);
            assert_eq!(problem.errors["capabilities"], [detail]);
        }
    }

    #[test]
    fn certification_results_distinguish_success_and_transport_failures() {
        let success =
            certification_item(capability(), Ok(CapabilityCertificationEvidence::LiveProbe));
        assert!(success.succeeded);
        assert!(success.error_code.is_none());
        assert!(success.detail.contains("production response codec"));

        for (class, code) in [
            (AttemptFailureClass::Connect, "connect_failed"),
            (AttemptFailureClass::Timeout, "timeout"),
            (AttemptFailureClass::RateLimit, "rate_limited"),
            (AttemptFailureClass::UpstreamServer, "upstream_server_error"),
            (
                AttemptFailureClass::UpstreamClient,
                "upstream_rejected_probe",
            ),
            (AttemptFailureClass::Protocol, "protocol_mismatch"),
            (AttemptFailureClass::Cancelled, "cancelled"),
            (AttemptFailureClass::Ambiguous, "ambiguous_result"),
        ] {
            let item = certification_item(
                capability(),
                Err(CompatibleCapabilityCertificationError::Transport {
                    phase: TransportPhase::FirstByte,
                    class,
                }),
            );
            assert!(!item.succeeded);
            assert_eq!(item.error_code.as_deref(), Some(code));
            assert_eq!(
                item.detail,
                "The live endpoint probe failed during FirstByte."
            );
        }

        for (error, code) in [
            (
                CompatibleCapabilityCertificationError::InvalidResult,
                "invalid_probe_result",
            ),
            (
                CompatibleCapabilityCertificationError::ModelNotDiscovered,
                "model_not_discovered",
            ),
        ] {
            let item = certification_item(capability(), Err(error));
            assert!(!item.succeeded);
            assert_eq!(item.error_code.as_deref(), Some(code));
        }
    }
}
