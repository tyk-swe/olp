use std::sync::Arc;

use crate::management::principal::MutationPrincipal;
use axum::{
    Json,
    extract::{Path, State, rejection::JsonRejection},
    http::{HeaderMap, StatusCode},
    response::Response,
};
use olp_db::{
    configuration::NewProviderDraft, idempotency::Outcome, idempotency::Replayable,
    idempotency::Response as IdempotencyResponse, idempotency::fingerprint,
    idempotency::operations, idempotency::secret_digest,
    security::aad::credential as credential_aad, security::envelope::MasterKey,
    store::RequestProvenance,
};
use olp_engine::domain::{
    auth::Permission,
    ports::ProviderTransport,
    provider::ProviderAuthMode,
    provider_configuration::{Configuration, provider_kind_spec, validate},
    routing::provider::ProviderKind,
};
use olp_engine::providers::factory::{assembly::Factory, configuration::Error};
use serde::{Deserialize, Serialize};
use tracing::error;
use utoipa::ToSchema;
use uuid::Uuid;

use crate::management::{
    error_mapping::{map_configuration, map_persistence},
    idempotency::{
        MutationReply, ReplayableMutation, idempotency_http_response, require_idempotency_key,
    },
    json_payload::json_payload,
    permissions::require_permission,
    preconditions::if_match,
    response_policy::RuntimeGenerationResponse,
    secrets::WriteOnlySecret,
};
use crate::{
    bootstrap::mode_dependencies::ManagementState,
    bootstrap::provider_adapter::{ProviderConfigFields, provider_config, provider_credential},
    public_http::problem::FieldErrorCodes,
    public_http::problem::FieldErrors,
    public_http::problem::Problem,
};

use super::manage::ProviderMutationFingerprint;
use super::record_violations;
use crate::management::provenance::Provenance;

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

fn validated_create_mode(
    request: &CreateProviderRequest,
) -> Result<(ProviderKind, ProviderAuthMode), Problem> {
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
    let auth_mode = request
        .auth_mode
        .unwrap_or_else(|| provider_kind_spec(kind).default_auth_mode);
    let mut codes = FieldErrorCodes::new();
    record_violations(
        validate(Configuration {
            kind,
            auth_mode,
            endpoint: request.endpoint.as_deref(),
            cloud_region: request.cloud_region.as_deref(),
            cloud_project: request.cloud_project.as_deref(),
            deployment: request.deployment.as_deref(),
            api_version: request.api_version.as_deref(),
            model: request.model.as_deref(),
            credential_present: Some(request.credential.is_some()),
        }),
        &mut errors,
        &mut codes,
    );
    if !errors.is_empty() {
        return Err(Problem::coded_validation(errors, codes));
    }
    Ok((kind, auth_mode))
}

async fn provisioned_provider_transport(
    state: &ManagementState,
    kind: ProviderKind,
    request: &CreateProviderRequest,
    auth_mode: ProviderAuthMode,
) -> Result<Arc<dyn ProviderTransport>, Problem> {
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
    Factory::transport(
        config,
        credential,
        &state.provider_egress_policy,
        state.provider_response_limits,
    )
    .await
    .map_err(|error| provider_connector_validation(kind, error))
}

struct PreparedProviderDraft {
    kind: ProviderKind,
    auth_mode: ProviderAuthMode,
    request_fingerprint: [u8; 32],
    idempotency_key: String,
    transport: Arc<dyn ProviderTransport>,
}

async fn persist_provider_draft(
    state: &ManagementState,
    provenance: &RequestProvenance,
    actor: Uuid,
    request: &CreateProviderRequest,
    master_key: &MasterKey,
    draft: PreparedProviderDraft,
) -> Result<Response, Problem> {
    let PreparedProviderDraft {
        kind,
        auth_mode,
        request_fingerprint,
        idempotency_key,
        transport,
    } = draft;
    let spec = provider_kind_spec(kind);
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
        .with_provenance(provenance)
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
                connector_ready: true,
                credential: encrypted,
                model: request.model.clone(),
                display_name: request.model.as_ref().map(|model| {
                    request
                        .display_name
                        .clone()
                        .unwrap_or_else(|| model.clone())
                }),
                model_enabled: request.model.is_some(),
                surface: request.model.as_ref().and(spec.seed_surface),
                actor,
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
                .and_then(|response| {
                    response.with_location(format!("/api/v1/providers/{}", created.provider_id))
                })
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
    path = "/api/v1/providers",
    tag = "providers",
    request_body = CreateProviderRequest,
    params(("Idempotency-Key" = String, Header, description = "Unique provider-draft creation key")),
    responses(
        (status = 201, description = "Provider draft created", body = ProviderResponse, headers(("Location" = String, description = "Path of the created resource"))),
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
    Provenance(provenance): Provenance,
    headers: HeaderMap,
    MutationPrincipal(principal): MutationPrincipal,
    payload: Result<Json<CreateProviderRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ManageProviders)?;
    let idempotency_key = require_idempotency_key(&headers)?.to_owned();
    let request = json_payload(payload)?;
    let request_fingerprint =
        fingerprint(&CreateProviderFingerprint::from(&request)).map_err(map_persistence)?;
    let master_key = state
        .master_key
        .as_deref()
        .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))?;
    let (kind, auth_mode) = validated_create_mode(&request)?;
    let transport = provisioned_provider_transport(&state, kind, &request, auth_mode).await?;
    persist_provider_draft(
        &state,
        &provenance,
        principal.user_id,
        &request,
        master_key,
        PreparedProviderDraft {
            kind,
            auth_mode,
            request_fingerprint,
            idempotency_key,
            transport,
        },
    )
    .await
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
    Provenance(provenance): Provenance,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
    MutationPrincipal(principal): MutationPrincipal,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ManageProviders)?;
    let expected_etag = if_match(&headers)?;
    let state = &state;
    let provenance = &provenance;
    ReplayableMutation::new(
        state,
        principal.user_id,
        operations::PROVIDER_ACTIVATE,
        &headers,
        &ProviderMutationFingerprint {
            provider_id,
            expected_etag,
        },
    )?
    .run(|key| async move {
        let activated = state
            .store()
            .with_provenance(provenance)
            .activate_provider(provider_id, expected_etag, principal.user_id, &key)
            .await
            .map_err(map_configuration)?;
        Ok(MutationReply {
            status: StatusCode::OK,
            body: ProviderActivationResponse {
                id: provider_id,
                state: "active".to_owned(),
                etag: activated.etag,
                runtime_generation: (&activated.release).into(),
            },
            etag: Some(activated.etag),
            location: None,
        })
    })
    .await
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
