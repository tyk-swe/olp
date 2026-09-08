use crate::access::policy::Permission;
use crate::database::idempotency::Replayable;
use crate::database::idempotency::Response as IdempotencyResponse;
use crate::database::idempotency::fingerprint;
use crate::protocols::canonical::identity::OperationKind;
use crate::providers::runtime_model::ProviderKind;
use crate::usage::pricing::PriceInput;
use crate::usage::pricing::RevisionRecord;
use axum::Json;
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
use utoipa::ToSchema;
use uuid::Uuid;

use crate::access::permissions::require_permission;
use crate::access::principal::MutationPrincipal;
use crate::access::principal::ReadPrincipal;
use crate::http::control::error_mapping::map_persistence;
use crate::http::control::idempotency::idempotency_http_response;
use crate::http::control::idempotency::require_idempotency_key;
use crate::http::control::json_payload::json_payload;
use crate::http::control::operations::helpers::map_operations;
use crate::http::control::pagination::PageQuery;
use crate::http::control::pagination::page_limit;
use crate::http::control::provenance::Provenance;
use crate::http::control::state::ManagementState;
use crate::http::problem::Problem;

#[derive(Clone, Copy, Debug, Deserialize, Serialize, ToSchema)]
#[serde(rename_all = "snake_case")]
pub(crate) enum PriceOperation {
    Generation,
    Embeddings,
    TokenCount,
    ImageGeneration,
    ImageEdit,
    ImageVariation,
    Speech,
    Transcription,
    VideoCreate,
    VideoList,
    VideoGet,
    VideoContent,
    VideoDelete,
    Moderation,
    ModelList,
    ModelGet,
}

impl From<PriceOperation> for OperationKind {
    fn from(value: PriceOperation) -> Self {
        match value {
            PriceOperation::Generation => Self::Generation,
            PriceOperation::Embeddings => Self::Embeddings,
            PriceOperation::TokenCount => Self::TokenCount,
            PriceOperation::ImageGeneration => Self::ImageGeneration,
            PriceOperation::ImageEdit => Self::ImageEdit,
            PriceOperation::ImageVariation => Self::ImageVariation,
            PriceOperation::Speech => Self::Speech,
            PriceOperation::Transcription => Self::Transcription,
            PriceOperation::VideoCreate => Self::VideoCreate,
            PriceOperation::VideoList => Self::VideoList,
            PriceOperation::VideoGet => Self::VideoGet,
            PriceOperation::VideoContent => Self::VideoContent,
            PriceOperation::VideoDelete => Self::VideoDelete,
            PriceOperation::Moderation => Self::Moderation,
            PriceOperation::ModelList => Self::ModelList,
            PriceOperation::ModelGet => Self::ModelGet,
        }
    }
}

#[derive(Debug, Deserialize, Serialize, ToSchema)]
pub(crate) struct PriceRequest {
    provider_kind: ProviderKind,
    #[schema(value_type = Option<String>, format = Uuid)]
    provider_id: Option<Uuid>,
    model: String,
    operation: PriceOperation,
    input_per_million: Option<String>,
    /// Rate for the cached share of the input tokens. Omit to bill cached
    /// tokens at the full input rate.
    cached_input_per_million: Option<String>,
    output_per_million: Option<String>,
    unit_price: Option<String>,
    currency: String,
}

impl From<PriceRequest> for PriceInput {
    fn from(price: PriceRequest) -> Self {
        Self {
            provider_kind: price.provider_kind,
            provider_id: price.provider_id,
            model: price.model,
            operation: OperationKind::from(price.operation),
            input_per_million: price.input_per_million,
            cached_input_per_million: price.cached_input_per_million,
            output_per_million: price.output_per_million,
            unit_price: price.unit_price,
            currency: price.currency,
        }
    }
}

#[derive(Debug, Deserialize, Serialize, ToSchema)]
pub(crate) struct PricingRevisionRequest {
    effective_at: DateTime<Utc>,
    prices: Vec<PriceRequest>,
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct PriceResponse {
    provider_kind: ProviderKind,
    #[schema(value_type = Option<String>, format = Uuid)]
    provider_id: Option<Uuid>,
    model: String,
    operation: String,
    input_per_million: Option<String>,
    /// Rate for the cached share of the input tokens. Omit to bill cached
    /// tokens at the full input rate.
    cached_input_per_million: Option<String>,
    output_per_million: Option<String>,
    unit_price: Option<String>,
    currency: String,
}

impl From<PriceInput> for PriceResponse {
    fn from(price: PriceInput) -> Self {
        Self {
            provider_kind: price.provider_kind,
            provider_id: price.provider_id,
            model: price.model,
            operation: price.operation.to_string(),
            input_per_million: price.input_per_million,
            cached_input_per_million: price.cached_input_per_million,
            output_per_million: price.output_per_million,
            unit_price: price.unit_price,
            currency: price.currency,
        }
    }
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct PricingRevisionResponse {
    #[schema(value_type = String, format = Uuid)]
    id: Uuid,
    revision: u32,
    effective_at: DateTime<Utc>,
    #[schema(value_type = String, format = Uuid)]
    created_by: Uuid,
    created_at: DateTime<Utc>,
    prices: Vec<PriceResponse>,
}

impl From<RevisionRecord> for PricingRevisionResponse {
    fn from(revision: RevisionRecord) -> Self {
        Self {
            id: revision.id,
            revision: revision.revision,
            effective_at: revision.effective_at,
            created_by: revision.created_by,
            created_at: revision.created_at,
            prices: revision.prices.into_iter().map(Into::into).collect(),
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct PricingRevisionsResponse {
    items: Vec<PricingRevisionResponse>,
    next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v3/pricing/revisions",
    tag = "pricing",
    params(PageQuery),
    responses(
        (status = 200, description = "Pricing revisions", body = PricingRevisionsResponse),
        (status = 400, description = "Malformed query parameters, or an invalid cursor or page size", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [])))]
pub(crate) async fn list_pricing_revisions(
    State(state): State<ManagementState>,
    Query(query): Query<PageQuery>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<PricingRevisionsResponse>, Problem> {
    require_permission(&principal, Permission::ReadOperations)?;
    let before = query
        .cursor
        .as_deref()
        .map(str::parse::<u32>)
        .transpose()
        .map_err(|_| Problem::bad_request("invalid_cursor", "The cursor is invalid."))?;
    let page = crate::usage::pricing::pricing_revisions_page(
        &state.request_boundary.pool,
        before,
        page_limit(query.limit)?,
    )
    .await
    .map_err(map_operations)?;
    let items = page.items.into_iter().map(Into::into).collect::<Vec<_>>();
    Ok(Json(PricingRevisionsResponse {
        items,
        next_cursor: page.next_cursor,
    }))
}

#[utoipa::path(
    post,
    path = "/api/v3/pricing/revisions",
    tag = "pricing",
    params(("Idempotency-Key" = String, Header, description = "Unique creation key")),
    request_body = PricingRevisionRequest,
    responses(
        (status = 201, description = "Pricing revision created", body = PricingRevisionResponse),
        (status = 400, description = "Idempotency-Key is missing or invalid", body = Problem, content_type = "application/problem+json"),
        (status = 409, description = "Idempotency key reused or request in progress", body = Problem, content_type = "application/problem+json"),
        (status = 422, description = "Invalid pricing revision", body = Problem, content_type = "application/problem+json"),
        (status = 503, description = "Master key or database unavailable", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [], "csrfToken" = [])))]
pub(crate) async fn create_pricing_revision(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    headers: HeaderMap,
    MutationPrincipal(principal): MutationPrincipal,
    payload: Result<Json<PricingRevisionRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ManagePricing)?;
    let idempotency_key = require_idempotency_key(&headers)?.to_owned();
    let request = json_payload(payload)?;
    let request_fingerprint = fingerprint(&request).map_err(map_persistence)?;
    let master_key = state
        .master_key
        .as_deref()
        .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))?;
    let prices = request
        .prices
        .into_iter()
        .map(Into::into)
        .collect::<Vec<_>>();
    let revision = crate::usage::pricing::create_pricing_revision(
        &state.request_boundary.pool,
        &provenance,
        principal.user_id,
        &idempotency_key,
        request.effective_at,
        &prices,
        Replayable::new(request_fingerprint, master_key),
        |revision| {
            IdempotencyResponse::json(
                StatusCode::CREATED.as_u16(),
                &PricingRevisionResponse::from(revision.clone()),
                None,
            )
        },
    )
    .await
    .map_err(map_operations)?;
    idempotency_http_response(revision)
}

pub(crate) fn router()
-> utoipa_axum::router::OpenApiRouter<crate::http::control::state::ManagementState> {
    utoipa_axum::router::OpenApiRouter::new()
        .routes(utoipa_axum::routes!(
            crate::usage::pricing_http::create_pricing_revision
        ))
        .routes(utoipa_axum::routes!(
            crate::usage::pricing_http::list_pricing_revisions
        ))
}
