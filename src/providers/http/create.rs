use std::sync::Arc;

use crate::access::policy::Permission;
use crate::access::principal::MutationPrincipal;
use crate::crypto::aad::credential as credential_aad;
use crate::crypto::envelope::MasterKey;
use crate::database::RequestProvenance;
use crate::database::idempotency::Outcome;
use crate::database::idempotency::Replayable;
use crate::database::idempotency::Response as IdempotencyResponse;
use crate::database::idempotency::fingerprint;
use crate::database::idempotency::operations;
use crate::database::idempotency::secret_digest;
use crate::inference::transport::ProviderTransport;
use crate::providers::connectors::configuration::Error;
use crate::providers::lifecycle::NewProviderDraft;
use crate::providers::runtime_model::ProviderKind;
use crate::providers::validation::provider_kind_spec;
use crate::providers::validation::validate;
use axum::Json;
use axum::extract::Path;
use axum::extract::State;
use axum::extract::rejection::JsonRejection;
use axum::http::HeaderMap;
use axum::http::StatusCode;
use axum::response::Response;
use serde::Deserialize;
use serde::Serialize;
use tracing::error;
use utoipa::ToSchema;
use uuid::Uuid;

use crate::access::permissions::require_permission;
use crate::http::control::error_mapping::map_configuration;
use crate::http::control::error_mapping::map_persistence;
use crate::http::control::idempotency::MutationReply;
use crate::http::control::idempotency::ReplayableMutation;
use crate::http::control::idempotency::idempotency_http_response;
use crate::http::control::idempotency::require_idempotency_key;
use crate::http::control::json_payload::json_payload;
use crate::http::control::preconditions::if_match;
use crate::http::control::response_policy::RuntimeGenerationResponse;
use crate::http::control::secrets::WriteOnlySecret;
use crate::http::control::state::ManagementState;
use crate::http::problem::FieldErrors;
use crate::http::problem::Problem;
use crate::providers::connectors::input::provider_credential;

use crate::http::control::provenance::Provenance;
use crate::providers::http::manage::ProviderMutationFingerprint;
use crate::providers::http::record_violations;

#[derive(Deserialize, ToSchema)]
#[serde(deny_unknown_fields)]
pub(crate) struct CreateProviderRequest {
    pub name: String,
    pub configuration: crate::providers::configuration::ProviderConfiguration,

    #[schema(value_type = String, write_only, required = false)]
    pub(crate) credential: Option<WriteOnlySecret>,
    /// Optional seed/probe model. Vertex AI requires one because its publisher
    /// model collection has no list operation; other connectors can discover
    /// models after the draft is created.
    pub model: Option<String>,
    pub display_name: Option<String>,
}

#[derive(Serialize)]
struct CreateProviderFingerprint<'a> {
    name: &'a str,
    configuration: &'a crate::providers::configuration::ProviderConfiguration,
    credential_sha256: Option<[u8; 32]>,
    model: Option<&'a str>,
    display_name: Option<&'a str>,
}

impl<'a> From<&'a CreateProviderRequest> for CreateProviderFingerprint<'a> {
    fn from(request: &'a CreateProviderRequest) -> Self {
        Self {
            name: &request.name,
            configuration: &request.configuration,
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

fn validated_create_mode(request: &CreateProviderRequest) -> Result<(), Problem> {
    let mut errors = FieldErrors::new();

    if request.name.trim().is_empty() || request.name.chars().count() > 100 {
        errors
            .entry("name".to_owned())
            .or_default()
            .push("Use between 1 and 100 characters.".to_owned().into());
    }
    if request
        .model
        .as_ref()
        .is_some_and(|model| model.trim().is_empty() || model.chars().count() > 200)
    {
        errors
            .entry("model".to_owned())
            .or_default()
            .push("Use between 1 and 200 characters.".to_owned().into());
    }
    if request.model.is_none() && request.display_name.is_some() {
        errors
            .entry("display_name".to_owned())
            .or_default()
            .push("A display name requires a seed model.".to_owned().into());
    }
    if request.credential.as_ref().is_some_and(|credential| {
        credential.expose().trim().is_empty() || credential.expose().len() > 8_192
    }) {
        errors.entry("credential".to_owned()).or_default().push(
            "Provide a credential no larger than 8 KiB."
                .to_owned()
                .into(),
        );
    }
    let mut configuration = request.configuration.clone();
    configuration.probe_model = request.model.clone();
    record_violations(
        validate(&configuration, Some(request.credential.is_some())),
        &mut errors,
    );
    if !errors.is_empty() {
        return Err(Problem::validation(errors));
    }
    Ok(())
}

async fn provisioned_provider_transport(
    state: &ManagementState,
    request: &CreateProviderRequest,
) -> Result<Arc<dyn ProviderTransport>, Problem> {
    let kind = request.configuration.kind;
    let mut config = request.configuration.clone();
    config.probe_model = request.model.clone();
    let credential = provider_credential(
        &config,
        request
            .credential
            .as_ref()
            .map(|credential| credential.expose().as_bytes()),
    )
    .map_err(|error| provider_connector_validation(config.kind, error))?;
    crate::providers::connectors::transport(
        config,
        credential,
        &state.provider_egress_policy,
        state.provider_response_limits,
    )
    .await
    .map_err(|error| provider_connector_validation(kind, error))
}

struct PreparedProviderDraft {
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
        request_fingerprint,
        idempotency_key,
        transport,
    } = draft;
    let spec = provider_kind_spec(request.configuration.kind);
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
    let response_kind = request.configuration.kind;
    let response_model = request.model.clone();
    let created = crate::providers::lifecycle::create_provider_draft(
        &state.request_boundary.pool,
        provenance,
        NewProviderDraft {
            provider_id,
            credential_id,
            model_id,
            name: request.name.clone(),
            configuration: request.configuration.clone(),

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
                response.with_location(format!("/api/v3/providers/{}", created.provider_id))
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
        state
            .transports
            .register(crate::ids::ProviderId::from_uuid(provider_id), transport);
    }
    idempotency_http_response(created)
}

#[utoipa::path(
    post,
    path = "/api/v3/providers",
    tag = "providers",
    request_body = CreateProviderRequest,
    params(("Idempotency-Key" = String, Header, description = "Unique provider-draft creation key")),
    responses(
        (status = 201, description = "Provider draft created", body = ProviderResponse, headers(("Location" = String, description = "Path of the created resource"))),
        (status = 400, description = "Idempotency-Key is missing or invalid", body = Problem, content_type = "application/problem+json"),
        (status = 401, description = "No active session", body = Problem, content_type = "application/problem+json"),
        (status = 403, description = "Insufficient role, CSRF, or origin failure", body = Problem, content_type = "application/problem+json"),
        (status = 409, description = "Idempotency-Key was already used or is in progress", body = Problem, content_type = "application/problem+json"),
        (status = 422, description = "Validation failed", body = Problem, content_type = "application/problem+json"),
        (status = 503, description = "Master key or database unavailable", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [], "csrfToken" = [])))]
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
    validated_create_mode(&request)?;
    let transport = provisioned_provider_transport(&state, &request).await?;
    persist_provider_draft(
        &state,
        &provenance,
        principal.user_id,
        &request,
        master_key,
        PreparedProviderDraft {
            request_fingerprint,
            idempotency_key,
            transport,
        },
    )
    .await
}

#[utoipa::path(
    post,
    path = "/api/v3/providers/{provider_id}/activate",
    tag = "providers",
    params(
        ("provider_id" = Uuid, Path, description = "Provider ID"),
        ("If-Match" = String, Header, description = "Current provider ETag"),
        ("Idempotency-Key" = String, Header, description = "Unique activation key")
    ),
    responses(
        (status = 200, description = "Provider activated", body = ProviderActivationResponse),
        (status = 400, description = "Required header is missing or invalid", body = Problem, content_type = "application/problem+json"),
        (status = 409, description = "Idempotency-Key was already used", body = Problem, content_type = "application/problem+json"),
        (status = 412, description = "ETag mismatch", body = Problem, content_type = "application/problem+json"),
        (status = 422, description = "Provider is incomplete or incompatible with live media jobs", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [], "csrfToken" = [])))]
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
        let activated = crate::providers::lifecycle::activate_provider(
            &state.request_boundary.pool,
            provenance,
            provider_id,
            expected_etag,
            principal.user_id,
            &key,
        )
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
