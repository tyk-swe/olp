use axum::{
    Json,
    extract::{Path, State, rejection::JsonRejection},
    http::{HeaderMap, StatusCode},
    response::Response,
};
use olp_db::{
    configuration::NewProviderDraft, idempotency::Outcome, idempotency::Replayable,
    idempotency::Response as IdempotencyResponse, idempotency::fingerprint,
    idempotency::secret_digest, security::aad::credential as credential_aad,
};
use olp_engine::domain::{
    provider::ProviderAuthMode,
    provider_configuration::{Configuration, provider_kind_spec, validate},
    routing::provider::ProviderKind,
};
use olp_engine::providers::factory::configuration::Error;
use serde::{Deserialize, Serialize};
use tracing::error;
use utoipa::ToSchema;
use uuid::Uuid;

use crate::management::{
    error_mapping::{map_configuration, map_persistence},
    idempotency::{idempotency_http_response, require_idempotency_key},
    json_payload::json_payload,
    permissions::require_provider_manager,
    preconditions::{if_match, with_etag},
    response_policy::RuntimeGenerationResponse,
    secrets::WriteOnlySecret,
    sessions::require_mutation_session,
};
use crate::{
    bootstrap::mode_dependencies::ManagementState,
    bootstrap::provider_adapter::{ProviderConfigFields, provider_config, provider_credential},
    public_http::problem::FieldErrors,
    public_http::problem::Problem,
};

#[derive(Deserialize, ToSchema)]
pub(crate) struct CreateProviderRequest {
    pub name: String,
    /// `openai` uses the official endpoint; `openai_compatible` requires an
    /// explicit HTTPS endpoint and live certification of reviewed capabilities.
    pub kind: ProviderKind,
    pub endpoint: Option<String>,
    pub cloud_region: Option<String>,
    pub cloud_project: Option<String>,
    pub deployment: Option<String>,
    pub api_version: Option<String>,
    pub auth_mode: Option<ProviderAuthMode>,
    #[schema(value_type = String, write_only, required = false)]
    pub(crate) credential: Option<WriteOnlySecret>,
    #[serde(rename = "api_key")]
    #[schema(ignore)]
    pub(crate) legacy_api_key: Option<WriteOnlySecret>,
    /// Optional seed/probe model. Vertex AI requires one because its publisher
    /// model collection has no list operation; other connectors can discover
    /// models after the draft is created.
    pub model: Option<String>,
    pub display_name: Option<String>,
}

#[derive(Serialize)]
struct CreateProviderFingerprint<'a> {
    name: &'a str,
    kind: ProviderKind,
    endpoint: Option<&'a str>,
    cloud_region: Option<&'a str>,
    cloud_project: Option<&'a str>,
    deployment: Option<&'a str>,
    api_version: Option<&'a str>,
    auth_mode: Option<ProviderAuthMode>,
    credential_sha256: Option<[u8; 32]>,
    model: Option<&'a str>,
    display_name: Option<&'a str>,
}

impl<'a> From<&'a CreateProviderRequest> for CreateProviderFingerprint<'a> {
    fn from(request: &'a CreateProviderRequest) -> Self {
        Self {
            name: &request.name,
            kind: request.kind,
            endpoint: request.endpoint.as_deref(),
            cloud_region: request.cloud_region.as_deref(),
            cloud_project: request.cloud_project.as_deref(),
            deployment: request.deployment.as_deref(),
            api_version: request.api_version.as_deref(),
            auth_mode: request.auth_mode,
            credential_sha256: request
                .credential
                .as_ref()
                .map(|credential| secret_digest(credential.expose().as_bytes())),
            model: request.model.as_deref(),
            display_name: request.display_name.as_deref(),
        }
    }
}

fn provider_connector_validation(kind: ProviderKind, error: Error) -> Problem {
    let (field, detail) = match error {
        Error::Configuration(detail) if kind == ProviderKind::Bedrock => ("cloud_region", detail),
        Error::Configuration(detail) => ("endpoint", detail),
        Error::Credential(detail) => ("credential", detail),
    };
    Problem::field_validation(field, detail)
}

fn reject_create_field(errors: &mut FieldErrors, field: &str, present: bool, detail: &str) {
    if present {
        errors
            .entry(field.to_owned())
            .or_default()
            .push(detail.to_owned());
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct ProviderResponse {
    #[schema(value_type = String, format = Uuid)]
    pub id: Uuid,
    pub name: String,
    pub kind: ProviderKind,
    pub state: String,
    pub model: Option<String>,
    #[schema(value_type = String, format = Uuid)]
    pub etag: Uuid,
}

#[utoipa::path(
    post,
    path = "/api/v1/providers",
    tag = "providers",
    request_body = CreateProviderRequest,
    params(("Idempotency-Key" = String, Header, description = "Unique provider-draft creation key")),
    responses(
        (status = 201, description = "Provider draft created", body = ProviderResponse),
        (status = 400, description = "Idempotency-Key is missing or invalid", body = Problem),
        (status = 401, description = "No active session", body = Problem),
        (status = 403, description = "Insufficient role, CSRF, or origin failure", body = Problem),
        (status = 409, description = "Idempotency-Key was already used or is in progress", body = Problem),
        (status = 422, description = "Validation failed", body = Problem),
        (status = 503, description = "Master key or database unavailable", body = Problem)
    )
)]
pub(crate) async fn create_provider(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    payload: Result<Json<CreateProviderRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_provider_manager(&principal)?;
    let idempotency_key = require_idempotency_key(&headers)?.to_owned();
    let request = json_payload(payload)?;
    let request_fingerprint =
        fingerprint(&CreateProviderFingerprint::from(&request)).map_err(map_persistence)?;
    let master_key = state
        .master_key
        .as_deref()
        .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))?;
    let mut errors = FieldErrors::new();
    reject_create_field(
        &mut errors,
        "api_key",
        request.legacy_api_key.is_some(),
        "api_key is no longer accepted; use credential.",
    );
    if request.name.trim().is_empty() || request.name.chars().count() > 100 {
        errors
            .entry("name".to_owned())
            .or_default()
            .push("Use between 1 and 100 characters.".to_owned());
    }
    if request
        .model
        .as_ref()
        .is_some_and(|model| model.trim().is_empty() || model.chars().count() > 200)
    {
        errors
            .entry("model".to_owned())
            .or_default()
            .push("Use between 1 and 200 characters.".to_owned());
    }
    if request.model.is_none() && request.display_name.is_some() {
        errors
            .entry("display_name".to_owned())
            .or_default()
            .push("A display name requires a seed model.".to_owned());
    }
    if request.credential.as_ref().is_some_and(|credential| {
        credential.expose().trim().is_empty() || credential.expose().len() > 8_192
    }) {
        errors
            .entry("credential".to_owned())
            .or_default()
            .push("Provide a credential no larger than 8 KiB.".to_owned());
    }
    let kind = request.kind;
    let spec = provider_kind_spec(kind);
    let auth_mode = request.auth_mode.unwrap_or(spec.default_auth_mode);
    for violation in validate(Configuration {
        kind,
        auth_mode,
        endpoint: request.endpoint.as_deref(),
        cloud_region: request.cloud_region.as_deref(),
        cloud_project: request.cloud_project.as_deref(),
        deployment: request.deployment.as_deref(),
        api_version: request.api_version.as_deref(),
        model: request.model.as_deref(),
        credential_present: Some(request.credential.is_some()),
    }) {
        errors
            .entry(violation.field.as_str().to_owned())
            .or_default()
            .push(violation.detail.to_owned());
    }
    if !errors.is_empty() {
        return Err(Problem::validation(errors));
    }
    let config = provider_config(ProviderConfigFields {
        kind,
        endpoint: request.endpoint.as_deref(),
        cloud_region: request.cloud_region.as_deref(),
        cloud_project: request.cloud_project.as_deref(),
        deployment: request.deployment.as_deref(),
        api_version: request.api_version.as_deref(),
        auth_mode,
        probe_model: request.model.as_deref(),
    })
    .map_err(|_| Problem::internal())?;
    let credential = provider_credential(
        &config,
        request
            .credential
            .as_ref()
            .map(|credential| credential.expose().as_bytes()),
    )
    .map_err(|error| provider_connector_validation(kind, error))?;
    let transport = crate::bootstrap::provider_adapter::factory_transport(config, credential)
        .await
        .map_err(|error| provider_connector_validation(kind, error))?;
    let connector_available = true;
    let provider_id = Uuid::now_v7();
    let credential_id = request.credential.as_ref().map(|_| Uuid::now_v7());
    let model_id = request.model.as_ref().map(|_| Uuid::now_v7());
    let encrypted = match (&request.credential, credential_id) {
        (Some(credential), Some(credential_id)) => Some(
            master_key
                .seal(
                    credential.expose().as_bytes(),
                    &credential_aad(provider_id, credential_id, 1),
                )
                .map_err(|error| {
                    error!(%error, "provider credential encryption failed");
                    Problem::internal()
                })?,
        ),
        (None, None) => None,
        _ => return Err(Problem::internal()),
    };
    let response_name = request.name.clone();
    let response_kind = request.kind;
    let response_model = request.model.clone();
    let created = state
        .store()
        .create_provider_draft(
            NewProviderDraft {
                provider_id,
                credential_id,
                model_id,
                name: request.name.clone(),
                kind,
                endpoint: request.endpoint.clone(),
                cloud_region: request.cloud_region.clone(),
                cloud_project: request.cloud_project.clone(),
                deployment: request.deployment.clone(),
                api_version: request.api_version.clone(),
                auth_mode,
                connector_ready: connector_available,
                credential: encrypted,
                model: request.model.clone(),
                display_name: request.model.as_ref().map(|model| {
                    request
                        .display_name
                        .clone()
                        .unwrap_or_else(|| model.clone())
                }),
                model_enabled: connector_available && request.model.is_some(),
                surface: request.model.as_ref().and(spec.seed_surface),
                actor: principal.user_id,
                idempotency_key,
            },
            Replayable::new(request_fingerprint, master_key),
            |created| {
                IdempotencyResponse::json(
                    StatusCode::CREATED.as_u16(),
                    &ProviderResponse {
                        id: created.provider_id,
                        name: response_name,
                        kind: response_kind,
                        state: "draft".to_owned(),
                        model: response_model,
                        etag: created.etag,
                    },
                    Some(format!("\"{}\"", created.etag)),
                )
            },
        )
        .await
        .map_err(map_configuration)?;
    let executed_provider_id = match &created {
        Outcome::Executed { value, .. } => Some(value.provider_id),
        Outcome::Replayed(_) => None,
    };
    if let Some(provider_id) = executed_provider_id {
        state.transports.register(
            olp_engine::domain::ids::ProviderId::from_uuid(provider_id),
            transport,
        );
    }
    idempotency_http_response(created)
}

#[utoipa::path(
    post,
    path = "/api/v1/providers/{provider_id}/activate",
    tag = "providers",
    params(
        ("provider_id" = Uuid, Path, description = "Provider ID"),
        ("If-Match" = String, Header, description = "Current provider ETag"),
        ("Idempotency-Key" = String, Header, description = "Unique activation key")
    ),
    responses(
        (status = 200, description = "Provider activated", body = ProviderActivationResponse),
        (status = 400, description = "Required header is missing or invalid", body = Problem),
        (status = 409, description = "Idempotency-Key was already used", body = Problem),
        (status = 412, description = "ETag mismatch", body = Problem),
        (status = 422, description = "Provider is incomplete", body = Problem)
    )
)]
pub(crate) async fn activate_provider(
    State(state): State<ManagementState>,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_provider_manager(&principal)?;
    let expected_etag = if_match(&headers)?;
    let idempotency_key = require_idempotency_key(&headers)?;
    let activated = state
        .store()
        .activate_provider(
            provider_id,
            expected_etag,
            principal.user_id,
            idempotency_key,
        )
        .await
        .map_err(map_configuration)?;
    with_etag(
        (
            StatusCode::OK,
            Json(ProviderActivationResponse {
                id: provider_id,
                state: "active".to_owned(),
                etag: activated.etag,
                runtime_generation: (&activated.release).into(),
            }),
        ),
        activated.etag,
    )
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct ProviderActivationResponse {
    #[schema(value_type = String, format = Uuid)]
    pub id: Uuid,
    pub state: String,
    #[schema(value_type = String, format = Uuid)]
    pub etag: Uuid,
    pub runtime_generation: RuntimeGenerationResponse,
}
