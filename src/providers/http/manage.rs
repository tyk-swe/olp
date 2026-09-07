use crate::access::policy::Permission;
use crate::database::idempotency::operations;
use crate::net::egress::EgressPolicy;
use crate::providers::records::ProviderRecord;
use crate::providers::records::UpdateProvider;
use crate::providers::runtime_model::ProviderKind;
use crate::providers::validation::validate;
use axum::Json;
use axum::extract::Path;
use axum::extract::Query;
use axum::extract::State;
use axum::extract::rejection::JsonRejection;
use axum::http::HeaderMap;
use axum::http::StatusCode;
use axum::response::Response;
use chrono::DateTime;
use chrono::Utc;
use serde::Deserialize;
use serde::Serialize;
use sqlx::PgPool;
use utoipa::ToSchema;
use uuid::Uuid;

use crate::access::permissions::require_permission;
use crate::access::principal::MutationPrincipal;
use crate::access::principal::ReadPrincipal;
use crate::http::control::error_mapping::map_configuration;
use crate::http::control::idempotency::MutationReply;
use crate::http::control::idempotency::ReplayableMutation;
use crate::http::control::json_payload::json_payload;
use crate::http::control::pagination::PageQuery;
use crate::http::control::pagination::page;
use crate::http::control::preconditions::if_match;
use crate::http::control::preconditions::with_etag;
use crate::http::control::state::ManagementState;
use crate::http::problem::FieldErrors;
use crate::http::problem::Problem;
use crate::providers::connect::provider_connector;

use crate::http::control::provenance::Provenance;
use crate::providers::http::credentials::ProviderMutationResponse;
use crate::providers::http::record_violations;

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ProviderSummaryResponse {
    pub id: Uuid,
    pub name: String,
    pub kind: ProviderKind,
    pub state: String,
    pub connector_ready: bool,
    pub etag: Uuid,
    pub active_revision: Option<u32>,
    pub pending_activation: bool,
    pub last_probe_at: Option<DateTime<Utc>>,
    pub last_probe_status: Option<String>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    pub model_count: u64,
    pub enabled_model_count: u64,
    pub capability_count: u64,
    pub certified_capability_count: u64,
    /// Email of the operator who created the provider.
    pub created_by_email: Option<String>,
}

/// The summary is the detail minus its connector and credential fields, so
/// the record-to-response mapping is written once, on the detail.
impl From<ProviderDetailResponse> for ProviderSummaryResponse {
    fn from(value: ProviderDetailResponse) -> Self {
        Self {
            id: value.id,
            name: value.name,
            kind: value.configuration.kind,
            state: value.state,
            connector_ready: value.connector_ready,
            etag: value.etag,
            active_revision: value.active_revision,
            pending_activation: value.pending_activation,
            last_probe_at: value.last_probe_at,
            last_probe_status: value.last_probe_status,
            created_at: value.created_at,
            updated_at: value.updated_at,
            model_count: value.model_count,
            enabled_model_count: value.enabled_model_count,
            capability_count: value.capability_count,
            certified_capability_count: value.certified_capability_count,
            created_by_email: value.created_by_email,
        }
    }
}

impl From<ProviderRecord> for ProviderSummaryResponse {
    fn from(value: ProviderRecord) -> Self {
        ProviderDetailResponse::from(value).into()
    }
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct ProviderDetailResponse {
    pub id: Uuid,
    pub name: String,
    pub configuration: crate::providers::configuration::ProviderConfiguration,
    pub state: String,

    pub connector_ready: bool,
    pub etag: Uuid,
    pub active_revision: Option<u32>,
    pub pending_activation: bool,
    pub draft_credential_id: Option<Uuid>,
    pub draft_credential_version: Option<i32>,
    pub runtime_credential_id: Option<Uuid>,
    pub runtime_credential_version: Option<i32>,
    pub last_probe_at: Option<DateTime<Utc>>,
    pub last_probe_status: Option<String>,
    pub last_probe_detail: Option<String>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    pub model_count: u64,
    pub enabled_model_count: u64,
    pub capability_count: u64,
    pub certified_capability_count: u64,
    /// Email of the operator who created the provider.
    pub created_by_email: Option<String>,
}

impl From<ProviderRecord> for ProviderDetailResponse {
    fn from(value: ProviderRecord) -> Self {
        Self {
            id: value.id,
            name: value.name,
            configuration: value.configuration,
            state: value.state.to_string(),

            connector_ready: value.connector_ready,
            etag: value.etag,
            active_revision: value.active_revision,
            pending_activation: value.pending_activation,
            draft_credential_id: value.draft_credential_id,
            draft_credential_version: value.draft_credential_version,
            runtime_credential_id: value.runtime_credential_id,
            runtime_credential_version: value.runtime_credential_version,
            last_probe_at: value.last_probe_at,
            last_probe_status: value.last_probe_status,
            last_probe_detail: value.last_probe_detail,
            created_at: value.created_at,
            updated_at: value.updated_at,
            model_count: value.model_count,
            enabled_model_count: value.enabled_model_count,
            capability_count: value.capability_count,
            certified_capability_count: value.certified_capability_count,
            created_by_email: value.created_by_email,
        }
    }
}

pub(crate) async fn load_provider_detail(
    pool: &PgPool,
    provider_id: Uuid,
) -> Result<ProviderDetailResponse, Problem> {
    crate::providers::repository::get_provider(pool, provider_id)
        .await
        .map(Into::into)
        .map_err(map_configuration)
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct ProviderListResponse {
    pub items: Vec<ProviderSummaryResponse>,
    pub next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v3/providers",
    tag = "providers",
    params(
        PageQuery,
    ),
    responses(
        (status = 200, body = ProviderListResponse),
        (status = 400, description = "Malformed query parameters, or an invalid cursor or page size", body = Problem, content_type = "application/problem+json"),
        (status = 401, body = Problem, content_type = "application/problem+json"),
        (status = 403, body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [])))]
pub(crate) async fn list_providers(
    State(state): State<ManagementState>,
    Query(query): Query<PageQuery>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<ProviderListResponse>, Problem> {
    require_permission(&principal, Permission::ReadConfiguration)?;
    let (cursor, limit) = page(query)?;
    let page =
        crate::providers::repository::list_providers(&state.request_boundary.pool, cursor, limit)
            .await
            .map_err(map_configuration)?;
    Ok(Json(ProviderListResponse {
        items: page.items.into_iter().map(Into::into).collect(),
        next_cursor: page.next_cursor.map(|value| value.to_string()),
    }))
}

#[utoipa::path(
    get,
    path = "/api/v3/providers/{provider_id}",
    tag = "providers",
    params(("provider_id" = Uuid, Path)),
    responses((status = 200, body = ProviderDetailResponse), (status = 404, body = Problem, content_type = "application/problem+json"))
, security(("sessionCookie" = [])))]
pub(crate) async fn get_provider(
    State(state): State<ManagementState>,
    Path(provider_id): Path<Uuid>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ReadConfiguration)?;
    let provider = load_provider_detail(&state.request_boundary.pool, provider_id).await?;
    let etag = provider.etag;
    with_etag(Json(provider), etag)
}

#[derive(Debug, Deserialize, ToSchema)]
pub(crate) struct UpdateProviderRequest {
    pub name: String,
    pub configuration: crate::providers::configuration::ProviderConfiguration,
}

#[utoipa::path(
    patch,
    path = "/api/v3/providers/{provider_id}",
    tag = "providers",
    params(("provider_id" = Uuid, Path), ("If-Match" = String, Header)),
    request_body = UpdateProviderRequest,
    responses((status = 200, body = ProviderDetailResponse), (status = 412, body = Problem, content_type = "application/problem+json"), (status = 422, body = Problem, content_type = "application/problem+json"))
, security(("sessionCookie" = [], "csrfToken" = [])))]
pub(crate) async fn update_provider(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
    MutationPrincipal(principal): MutationPrincipal,
    payload: Result<Json<UpdateProviderRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ManageProviders)?;
    let request = json_payload(payload)?;
    let pool = &state.request_boundary.pool;
    let current = crate::providers::repository::get_provider(pool, provider_id)
        .await
        .map_err(map_configuration)?;
    validate_provider_update(&current, &request, &state.provider_egress_policy)?;
    let etag = crate::providers::repository::update_provider(
        pool,
        &provenance,
        provider_id,
        if_match(&headers)?,
        &UpdateProvider {
            name: request.name,
            configuration: request.configuration,
        },
        principal.user_id,
    )
    .await
    .map_err(map_configuration)?;
    let provider = load_provider_detail(pool, provider_id).await?;
    with_etag(Json(provider), etag)
}

#[utoipa::path(
    post,
    path = "/api/v3/providers/{provider_id}/disable",
    tag = "providers",
    params(
        ("provider_id" = Uuid, Path),
        ("If-Match" = String, Header),
        ("Idempotency-Key" = String, Header)
    ),
    responses(
        (status = 200, body = ProviderMutationResponse),
        (status = 400, description = "Idempotency-Key is missing or invalid", body = Problem, content_type = "application/problem+json"),
        (status = 409, description = "Provider is still referenced by an active route, or the Idempotency-Key was already used for a different request", body = Problem, content_type = "application/problem+json"),
        (status = 412, body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [], "csrfToken" = [])))]
pub(crate) async fn disable_provider(
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
        operations::PROVIDER_DISABLE,
        &headers,
        &ProviderMutationFingerprint {
            provider_id,
            expected_etag,
        },
    )?
    .run(|key| async move {
        let result = crate::providers::repository::disable_provider(
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
            body: ProviderMutationResponse {
                provider_id,
                etag: result.etag,
                credential_id: None,
                credential_version: None,
                runtime_generation: result.release.as_ref().map(Into::into),
            },
            etag: Some(result.etag),
            location: None,
        })
    })
    .await
}

#[derive(Serialize)]
pub(crate) struct ProviderMutationFingerprint {
    pub provider_id: Uuid,
    pub expected_etag: Uuid,
}

#[utoipa::path(
    post,
    path = "/api/v3/providers/{provider_id}/restore-as-draft",
    tag = "providers",
    params(
        ("provider_id" = Uuid, Path),
        ("If-Match" = String, Header),
        ("Idempotency-Key" = String, Header)
    ),
    responses(
        (status = 200, body = ProviderDetailResponse),
        (status = 400, description = "Idempotency-Key is missing or invalid", body = Problem, content_type = "application/problem+json"),
        (status = 409, description = "Idempotency-Key was already used or is in progress", body = Problem, content_type = "application/problem+json"),
        (status = 412, body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [], "csrfToken" = [])))]
pub(crate) async fn restore_provider_as_draft(
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
        operations::PROVIDER_RESTORE_AS_DRAFT,
        &headers,
        &ProviderMutationFingerprint {
            provider_id,
            expected_etag,
        },
    )?
    .run(|key| async move {
        let pool = &state.request_boundary.pool;
        let etag = crate::providers::repository::restore_provider_as_draft(
            pool,
            provenance,
            provider_id,
            expected_etag,
            principal.user_id,
            &key,
        )
        .await
        .map_err(map_configuration)?;
        let provider = load_provider_detail(pool, provider_id).await?;
        Ok(MutationReply {
            status: StatusCode::OK,
            body: provider,
            etag: Some(etag),
            location: None,
        })
    })
    .await
}

fn validate_provider_update(
    provider: &ProviderRecord,
    request: &UpdateProviderRequest,
    egress_policy: &EgressPolicy,
) -> Result<(), Problem> {
    if request.configuration.kind != provider.configuration.kind {
        return Err(Problem::field_validation(
            "kind",
            "Provider kind is immutable.",
        ));
    }
    if request.configuration.auth_mode != provider.configuration.auth_mode {
        return Err(Problem::field_validation(
            "auth_mode",
            "Provider authentication mode is immutable; create a separate provider to change identity mode.",
        ));
    }

    let mut config = request.configuration.clone();
    config.probe_model = provider.configuration.probe_model.clone();
    let mut errors = FieldErrors::new();
    record_violations(
        validate(&config, Some(provider.draft_credential_id.is_some())),
        &mut errors,
    );
    if !errors.is_empty() {
        return Err(Problem::validation(errors));
    }

    crate::providers::connectors::configuration::validate_connector_configuration(
        &config,
        egress_policy,
    )
    .map_err(|error| Problem::field_validation("provider", error.to_string()))
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct ProbeResponse {
    pub provider_id: Uuid,
    pub succeeded: bool,
    pub checked_at: DateTime<Utc>,
    pub probe_type: String,
    pub detail: String,
    pub discovered_models: Option<usize>,
}

#[utoipa::path(
    post,
    path = "/api/v3/providers/{provider_id}/probe",
    tag = "providers",
    params(
        ("provider_id" = Uuid, Path),
        ("If-Match" = String, Header, description = "Exact provider draft ETag being probed")
    ),
    responses(
        (status = 200, body = ProbeResponse),
        (status = 412, description = "Provider changed before probe evidence could be recorded", body = Problem, content_type = "application/problem+json"),
        (status = 422, body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [], "csrfToken" = [])))]
pub(crate) async fn probe_provider(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    Path(provider_id): Path<Uuid>,
    headers: HeaderMap,
    MutationPrincipal(principal): MutationPrincipal,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ManageProviders)?;
    let pool = &state.request_boundary.pool;
    let expected_etag = if_match(&headers)?;
    let provider = crate::providers::repository::get_provider(pool, provider_id)
        .await
        .map_err(map_configuration)?;
    if provider.etag != expected_etag {
        return Err(map_configuration(
            crate::providers::error::Error::PreconditionFailed,
        ));
    }
    let connector = provider_connector(&state, provider_id).await?;
    // Configuration-only checks are intentionally not accepted as activation
    // evidence. A probe always performs a bounded credentialed upstream call,
    // and persistence binds the result to the exact ETag captured above.
    let probe = connector.discover_models().await;
    let (succeeded, detail, discovered_models) = match probe {
        Ok(models) => (
            true,
            "Credentialed connector request succeeded.".to_owned(),
            Some(models.len()),
        ),
        Err(detail) => (false, detail, None),
    };
    let checked_at = crate::providers::repository::record_provider_probe(
        pool,
        &provenance,
        provider_id,
        expected_etag,
        succeeded,
        &detail,
        principal.user_id,
    )
    .await
    .map_err(map_configuration)?;
    if !succeeded {
        return Err(Problem::field_validation("provider", detail));
    }
    with_etag(
        Json(ProbeResponse {
            provider_id,
            succeeded,
            checked_at,
            probe_type: if provider.configuration.kind == ProviderKind::AzureOpenAi {
                "deployment_capability".to_owned()
            } else {
                "connector_connectivity".to_owned()
            },
            detail,
            discovered_models,
        }),
        expected_etag,
    )
}
