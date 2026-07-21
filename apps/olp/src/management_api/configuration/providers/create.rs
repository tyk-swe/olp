use axum::{
    Json,
    extract::{Path, State, rejection::JsonRejection},
    http::{HeaderMap, HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
};
use olp_domain::ProviderKind;
use olp_providers::{ProviderError, ProviderFactory};
use olp_storage::{
    IdempotencyOutcome, IdempotencyResponse, NewProviderDraft, ReplayableIdempotency,
    credential_aad, idempotency_fingerprint, idempotency_secret_digest,
};
use serde::{Deserialize, Serialize};
use tracing::error;
use utoipa::ToSchema;
use uuid::Uuid;

use crate::{
    ApiState, FieldErrors, Problem,
    management_api::common::*,
    provider_adapter::{ProviderConfigFields, provider_config, provider_credential},
};

#[derive(Deserialize, ToSchema)]
pub(crate) struct CreateProviderRequest {
    pub name: String,
    /// `openai` uses the official endpoint; `openai_compatible` requires an
    /// explicit HTTPS endpoint and live certification of reviewed capabilities.
    pub kind: String,
    pub endpoint: Option<String>,
    pub cloud_region: Option<String>,
    pub cloud_project: Option<String>,
    pub deployment: Option<String>,
    pub api_version: Option<String>,
    pub auth_mode: Option<String>,
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
    kind: &'a str,
    endpoint: Option<&'a str>,
    cloud_region: Option<&'a str>,
    cloud_project: Option<&'a str>,
    deployment: Option<&'a str>,
    api_version: Option<&'a str>,
    auth_mode: Option<&'a str>,
    credential_sha256: Option<[u8; 32]>,
    model: Option<&'a str>,
    display_name: Option<&'a str>,
}

impl<'a> From<&'a CreateProviderRequest> for CreateProviderFingerprint<'a> {
    fn from(request: &'a CreateProviderRequest) -> Self {
        Self {
            name: &request.name,
            kind: &request.kind,
            endpoint: request.endpoint.as_deref(),
            cloud_region: request.cloud_region.as_deref(),
            cloud_project: request.cloud_project.as_deref(),
            deployment: request.deployment.as_deref(),
            api_version: request.api_version.as_deref(),
            auth_mode: request.auth_mode.as_deref(),
            credential_sha256: request
                .credential
                .as_ref()
                .map(|credential| idempotency_secret_digest(credential.expose().as_bytes())),
            model: request.model.as_deref(),
            display_name: request.display_name.as_deref(),
        }
    }
}

fn connector_validation(error: impl ToString) -> Problem {
    let mut errors = FieldErrors::new();
    errors.insert("endpoint".to_owned(), vec![error.to_string()]);
    Problem::validation(errors)
}

fn connector_credential_validation(error: impl ToString) -> Problem {
    let mut errors = FieldErrors::new();
    errors.insert("credential".to_owned(), vec![error.to_string()]);
    Problem::validation(errors)
}

fn bedrock_region_validation(error: impl ToString) -> Problem {
    let mut errors = FieldErrors::new();
    errors.insert("cloud_region".to_owned(), vec![error.to_string()]);
    Problem::validation(errors)
}

fn bedrock_credential_validation(error: impl ToString) -> Problem {
    let mut errors = FieldErrors::new();
    errors.insert("credential".to_owned(), vec![error.to_string()]);
    Problem::validation(errors)
}

fn provider_connector_validation(kind: ProviderKind, error: ProviderError) -> Problem {
    match error {
        ProviderError::Configuration(detail) if kind == ProviderKind::Bedrock => {
            bedrock_region_validation(detail)
        }
        ProviderError::Configuration(detail) => connector_validation(detail),
        ProviderError::Credential(detail) if kind == ProviderKind::Bedrock => {
            bedrock_credential_validation(detail)
        }
        ProviderError::Credential(detail) => connector_credential_validation(detail),
    }
}

pub(crate) fn reject_create_field(
    errors: &mut FieldErrors,
    field: &str,
    present: bool,
    detail: &str,
) {
    if present {
        errors
            .entry(field.to_owned())
            .or_default()
            .push(detail.to_owned());
    }
}

pub(crate) fn reject_create_cloud_fields(
    errors: &mut FieldErrors,
    request: &CreateProviderRequest,
) {
    for (field, present) in [
        ("cloud_region", request.cloud_region.is_some()),
        ("cloud_project", request.cloud_project.is_some()),
        ("deployment", request.deployment.is_some()),
        ("api_version", request.api_version.is_some()),
    ] {
        reject_create_field(
            errors,
            field,
            present,
            "This connector does not accept cloud project, region, deployment, or API-version fields.",
        );
    }
}

pub(crate) fn require_create_auth_mode(errors: &mut FieldErrors, actual: &str, expected: &str) {
    if actual != expected {
        errors
            .entry("auth_mode".to_owned())
            .or_default()
            .push(format!("Provider authentication must be {expected}."));
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct ProviderResponse {
    #[schema(value_type = String, format = Uuid)]
    pub id: Uuid,
    pub name: String,
    pub kind: String,
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
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<CreateProviderRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_provider_manager(&principal)?;
    let idempotency_key = require_idempotency_key(&headers)?.to_owned();
    let request = json_payload(payload)?;
    let request_fingerprint = idempotency_fingerprint(&CreateProviderFingerprint::from(&request))
        .map_err(map_persistence)?;
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
    let (kind, base_url, surface) = match request.kind.as_str() {
        "openai" => (
            ProviderKind::OpenAi,
            request.endpoint.clone().unwrap_or_default(),
            Some("openai"),
        ),
        "openai_compatible" => {
            if let Some(endpoint) = request.endpoint.clone() {
                (ProviderKind::OpenAiCompatible, endpoint, Some("openai"))
            } else {
                errors
                    .entry("endpoint".to_owned())
                    .or_default()
                    .push("An HTTPS endpoint is required.".to_owned());
                (
                    ProviderKind::OpenAiCompatible,
                    String::new(),
                    Some("openai"),
                )
            }
        }
        "anthropic" => (
            ProviderKind::Anthropic,
            request.endpoint.clone().unwrap_or_default(),
            Some("anthropic"),
        ),
        "gemini" => (
            ProviderKind::Gemini,
            request.endpoint.clone().unwrap_or_default(),
            Some("gemini"),
        ),
        "vertex_ai" => (
            ProviderKind::VertexAi,
            request.endpoint.clone().unwrap_or_default(),
            Some("gemini"),
        ),
        "azure_openai" => (
            ProviderKind::AzureOpenAi,
            request.endpoint.clone().unwrap_or_default(),
            Some("openai"),
        ),
        "bedrock" => (
            ProviderKind::Bedrock,
            request.endpoint.clone().unwrap_or_default(),
            None,
        ),
        _ => {
            errors
                .entry("kind".to_owned())
                .or_default()
                .push(
                    "Use openai, openai_compatible, anthropic, gemini, vertex_ai, azure_openai, or bedrock."
                        .to_owned(),
                );
            (ProviderKind::OpenAi, String::new(), Some("openai"))
        }
    };
    let auth_mode = request.auth_mode.clone().unwrap_or_else(|| match kind {
        ProviderKind::VertexAi => "adc".to_owned(),
        ProviderKind::Bedrock => "default_chain".to_owned(),
        _ => "api_key".to_owned(),
    });
    let credential_required = matches!(
        kind,
        ProviderKind::OpenAi
            | ProviderKind::OpenAiCompatible
            | ProviderKind::Anthropic
            | ProviderKind::Gemini
            | ProviderKind::AzureOpenAi
    ) || matches!(
        (kind, auth_mode.as_str()),
        (ProviderKind::VertexAi, "service_account") | (ProviderKind::Bedrock, "static")
    );
    if credential_required && request.credential.is_none() {
        errors
            .entry("credential".to_owned())
            .or_default()
            .push("This authentication mode requires a write-only credential.".to_owned());
    }
    match kind {
        ProviderKind::OpenAi => {
            require_create_auth_mode(&mut errors, &auth_mode, "api_key");
            reject_create_field(
                &mut errors,
                "endpoint",
                request.endpoint.is_some(),
                "Native OpenAI uses the official endpoint; use an OpenAI-compatible provider for a custom endpoint.",
            );
            reject_create_cloud_fields(&mut errors, &request);
        }
        ProviderKind::OpenAiCompatible => {
            require_create_auth_mode(&mut errors, &auth_mode, "api_key");
            reject_create_cloud_fields(&mut errors, &request);
        }
        ProviderKind::Anthropic => {
            require_create_auth_mode(&mut errors, &auth_mode, "api_key");
            reject_create_field(
                &mut errors,
                "endpoint",
                request.endpoint.is_some(),
                "Native Anthropic uses the official endpoint.",
            );
            reject_create_cloud_fields(&mut errors, &request);
        }
        ProviderKind::Gemini => {
            require_create_auth_mode(&mut errors, &auth_mode, "api_key");
            reject_create_field(
                &mut errors,
                "endpoint",
                request.endpoint.is_some(),
                "Gemini Developer API uses the official endpoint.",
            );
            reject_create_cloud_fields(&mut errors, &request);
        }
        ProviderKind::VertexAi => {
            if request.model.is_none() {
                errors
                    .entry("model".to_owned())
                    .or_default()
                    .push("Vertex AI requires an explicit model to probe.".to_owned());
            }
            if request.cloud_project.as_deref().is_none_or(str::is_empty) {
                errors
                    .entry("cloud_project".to_owned())
                    .or_default()
                    .push("Vertex AI requires a cloud project.".to_owned());
            }
            if request.cloud_region.as_deref().is_none_or(str::is_empty) {
                errors
                    .entry("cloud_region".to_owned())
                    .or_default()
                    .push("Vertex AI requires a cloud region.".to_owned());
            }
            if !matches!(auth_mode.as_str(), "adc" | "service_account") {
                errors
                    .entry("auth_mode".to_owned())
                    .or_default()
                    .push("Use adc or service_account for Vertex AI.".to_owned());
            }
            if auth_mode == "adc" && request.credential.is_some() {
                errors
                    .entry("credential".to_owned())
                    .or_default()
                    .push("Do not submit a credential when using Vertex ADC.".to_owned());
            }
            if request.endpoint.is_some() {
                errors
                    .entry("endpoint".to_owned())
                    .or_default()
                    .push(
                        "Vertex AI derives its regional Google endpoint from cloud_project and cloud_region."
                            .to_owned(),
                    );
            }
            reject_create_field(
                &mut errors,
                "deployment",
                request.deployment.is_some(),
                "Vertex AI does not accept a deployment field.",
            );
            reject_create_field(
                &mut errors,
                "api_version",
                request.api_version.is_some(),
                "Vertex AI does not accept an API-version field.",
            );
        }
        ProviderKind::Bedrock => {
            if request.cloud_region.as_deref().is_none_or(str::is_empty) {
                errors
                    .entry("cloud_region".to_owned())
                    .or_default()
                    .push("Bedrock requires an AWS region.".to_owned());
            }
            if !matches!(auth_mode.as_str(), "default_chain" | "static") {
                errors
                    .entry("auth_mode".to_owned())
                    .or_default()
                    .push("Use default_chain or static for Bedrock.".to_owned());
            }
            if request.endpoint.is_some() {
                errors
                    .entry("endpoint".to_owned())
                    .or_default()
                    .push(
                        "Bedrock uses the official regional AWS endpoint; custom endpoints are not accepted."
                            .to_owned(),
                    );
            }
            reject_create_field(
                &mut errors,
                "cloud_project",
                request.cloud_project.is_some(),
                "Bedrock does not accept a cloud project.",
            );
            reject_create_field(
                &mut errors,
                "deployment",
                request.deployment.is_some(),
                "Bedrock does not accept a deployment field.",
            );
            reject_create_field(
                &mut errors,
                "api_version",
                request.api_version.is_some(),
                "Bedrock does not accept an API-version field.",
            );
            if auth_mode == "default_chain" && request.credential.is_some() {
                errors.entry("credential".to_owned()).or_default().push(
                    "Do not submit a credential when using the AWS default chain.".to_owned(),
                );
            }
        }
        ProviderKind::AzureOpenAi => {
            if base_url.is_empty() {
                errors
                    .entry("endpoint".to_owned())
                    .or_default()
                    .push("Azure OpenAI requires an HTTPS resource endpoint.".to_owned());
            }
            if request.deployment.as_deref().is_none_or(str::is_empty) {
                errors
                    .entry("deployment".to_owned())
                    .or_default()
                    .push("Azure OpenAI requires a deployment name.".to_owned());
            }
            if request.api_version.as_deref().is_none_or(str::is_empty) {
                errors
                    .entry("api_version".to_owned())
                    .or_default()
                    .push("Azure OpenAI requires an API version.".to_owned());
            }
            if auth_mode != "api_key" {
                errors
                    .entry("auth_mode".to_owned())
                    .or_default()
                    .push("Azure OpenAI currently requires api_key authentication.".to_owned());
            }
            reject_create_field(
                &mut errors,
                "cloud_region",
                request.cloud_region.is_some(),
                "Azure OpenAI does not accept a cloud region.",
            );
            reject_create_field(
                &mut errors,
                "cloud_project",
                request.cloud_project.is_some(),
                "Azure OpenAI does not accept a cloud project.",
            );
        }
    }
    if !errors.is_empty() {
        return Err(Problem::validation(errors));
    }
    let parsed_auth_mode = auth_mode.parse().map_err(|_| Problem::internal())?;
    let config = provider_config(ProviderConfigFields {
        kind,
        endpoint: matches!(
            kind,
            ProviderKind::OpenAiCompatible | ProviderKind::AzureOpenAi
        )
        .then_some(base_url.as_str()),
        cloud_region: request.cloud_region.as_deref(),
        cloud_project: request.cloud_project.as_deref(),
        deployment: request.deployment.as_deref(),
        api_version: request.api_version.as_deref(),
        auth_mode: parsed_auth_mode,
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
    let transport = ProviderFactory::transport(config, credential)
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
    let response_kind = request.kind.clone();
    let response_model = request.model.clone();
    let created = require_store(&state)?
        .create_provider_draft(
            NewProviderDraft {
                provider_id,
                credential_id,
                model_id,
                name: request.name.clone(),
                kind,
                endpoint: matches!(
                    kind,
                    ProviderKind::OpenAiCompatible | ProviderKind::AzureOpenAi
                )
                .then_some(base_url),
                cloud_region: request.cloud_region.clone(),
                cloud_project: request.cloud_project.clone(),
                deployment: request.deployment.clone(),
                api_version: request.api_version.clone(),
                auth_mode: auth_mode.parse().map_err(|_| Problem::internal())?,
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
                surface: request
                    .model
                    .as_ref()
                    .and(surface)
                    .map(str::parse)
                    .transpose()
                    .map_err(|_| Problem::internal())?,
                actor: principal.user_id,
                idempotency_key,
            },
            ReplayableIdempotency::new(request_fingerprint, master_key),
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
        IdempotencyOutcome::Executed { value, .. } => Some(value.provider_id),
        IdempotencyOutcome::Replayed(_) => None,
    };
    if let Some(provider_id) = executed_provider_id {
        state
            .transports
            .register(olp_domain::ProviderId::from_uuid(provider_id), transport);
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
    State(state): State<ApiState>,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_provider_manager(&principal)?;
    let expected_etag = if_match(&headers)?;
    let idempotency_key = require_idempotency_key(&headers)?;
    let activated = require_store(&state)?
        .activate_provider(
            provider_id,
            expected_etag,
            principal.user_id,
            idempotency_key,
        )
        .await
        .map_err(map_configuration)?;
    let mut response = (
        StatusCode::OK,
        Json(ProviderActivationResponse {
            id: provider_id,
            state: "active".to_owned(),
            etag: activated.etag,
            runtime_generation: RuntimeGenerationResponse {
                id: activated.release.generation_id,
                sequence: activated.release.sequence,
            },
        }),
    )
        .into_response();
    response.headers_mut().insert(
        header::ETAG,
        HeaderValue::from_str(&format!("\"{}\"", activated.etag))
            .map_err(|_| Problem::internal())?,
    );
    Ok(response)
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
