use axum::{
    Json,
    extract::{Query, State, rejection::JsonRejection},
    http::{HeaderMap, StatusCode},
    response::Response,
};
use chrono::{DateTime, Utc};
use olp_domain::{OperationKind, ProviderKind};
use olp_storage::{
    IdempotencyResponse, PriceInput, PricingRevisionRecord, ReplayableIdempotency,
    idempotency_fingerprint,
};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;
use uuid::Uuid;

use super::helpers::{PageQuery, map_operations, page_limit};
use crate::{
    ApiState, Problem,
    management_api::{
        Permission, idempotency_http_response, json_payload, map_persistence,
        require_idempotency_key, require_mutation_session, require_permission,
        require_read_session, require_store,
    },
};

#[derive(Clone, Copy, Debug, Deserialize, Serialize, ToSchema)]
#[serde(rename_all = "snake_case")]
pub(super) enum PriceProviderKind {
    #[serde(rename = "openai")]
    OpenAi,
    Anthropic,
    Gemini,
    VertexAi,
    Bedrock,
    #[serde(rename = "azure_openai")]
    AzureOpenAi,
    #[serde(rename = "openai_compatible")]
    OpenAiCompatible,
}

impl From<PriceProviderKind> for ProviderKind {
    fn from(value: PriceProviderKind) -> Self {
        match value {
            PriceProviderKind::OpenAi => Self::OpenAi,
            PriceProviderKind::Anthropic => Self::Anthropic,
            PriceProviderKind::Gemini => Self::Gemini,
            PriceProviderKind::VertexAi => Self::VertexAi,
            PriceProviderKind::Bedrock => Self::Bedrock,
            PriceProviderKind::AzureOpenAi => Self::AzureOpenAi,
            PriceProviderKind::OpenAiCompatible => Self::OpenAiCompatible,
        }
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, ToSchema)]
#[serde(rename_all = "snake_case")]
pub(super) enum PriceOperation {
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
pub(super) struct PriceRequest {
    provider_kind: PriceProviderKind,
    #[schema(value_type = Option<String>, format = Uuid)]
    provider_id: Option<Uuid>,
    model: String,
    operation: PriceOperation,
    input_per_million: Option<String>,
    output_per_million: Option<String>,
    unit_price: Option<String>,
    currency: String,
}

impl From<PriceRequest> for PriceInput {
    fn from(price: PriceRequest) -> Self {
        Self {
            provider_kind: ProviderKind::from(price.provider_kind),
            provider_id: price.provider_id,
            model: price.model,
            operation: OperationKind::from(price.operation),
            input_per_million: price.input_per_million,
            output_per_million: price.output_per_million,
            unit_price: price.unit_price,
            currency: price.currency,
        }
    }
}

#[derive(Debug, Deserialize, Serialize, ToSchema)]
pub(super) struct PricingRevisionRequest {
    effective_at: DateTime<Utc>,
    prices: Vec<PriceRequest>,
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct PriceResponse {
    provider_kind: String,
    #[schema(value_type = Option<String>, format = Uuid)]
    provider_id: Option<Uuid>,
    model: String,
    operation: String,
    input_per_million: Option<String>,
    output_per_million: Option<String>,
    unit_price: Option<String>,
    currency: String,
}

impl From<PriceInput> for PriceResponse {
    fn from(price: PriceInput) -> Self {
        Self {
            provider_kind: price.provider_kind.to_string(),
            provider_id: price.provider_id,
            model: price.model,
            operation: price.operation.to_string(),
            input_per_million: price.input_per_million,
            output_per_million: price.output_per_million,
            unit_price: price.unit_price,
            currency: price.currency,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct PricingRevisionResponse {
    #[schema(value_type = String, format = Uuid)]
    id: Uuid,
    revision: u32,
    effective_at: DateTime<Utc>,
    #[schema(value_type = String, format = Uuid)]
    created_by: Uuid,
    created_at: DateTime<Utc>,
    prices: Vec<PriceResponse>,
}

impl From<PricingRevisionRecord> for PricingRevisionResponse {
    fn from(revision: PricingRevisionRecord) -> Self {
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
pub(super) struct PricingRevisionsResponse {
    data: Vec<PricingRevisionResponse>,
    next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/pricing/revisions",
    tag = "pricing",
    params(PageQuery),
    responses((status = 200, description = "Pricing revisions", body = PricingRevisionsResponse))
)]
pub(super) async fn list_pricing_revisions(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<PricingRevisionsResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    let before = query
        .cursor
        .as_deref()
        .map(str::parse::<u32>)
        .transpose()
        .map_err(|_| Problem::bad_request("invalid_cursor", "The cursor is invalid."))?;
    let page = require_store(&state)?
        .pricing_revisions_page(before, page_limit(query.limit)?)
        .await
        .map_err(map_operations)?;
    Ok(Json(PricingRevisionsResponse {
        data: page.items.into_iter().map(Into::into).collect(),
        next_cursor: page.next_cursor,
    }))
}

#[utoipa::path(
    post,
    path = "/api/v1/pricing/revisions",
    tag = "pricing",
    params(("Idempotency-Key" = String, Header, description = "Unique creation key")),
    request_body = PricingRevisionRequest,
    responses(
        (status = 201, description = "Pricing revision created", body = PricingRevisionResponse),
        (status = 409, description = "Idempotency key reused or request in progress", body = Problem),
        (status = 422, description = "Invalid pricing revision", body = Problem),
        (status = 503, description = "Master key or database unavailable", body = Problem)
    )
)]
pub(super) async fn create_pricing_revision(
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<PricingRevisionRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManagePricing)?;
    let idempotency_key = require_idempotency_key(&headers)?.to_owned();
    let request = json_payload(payload)?;
    let request_fingerprint = idempotency_fingerprint(&request).map_err(map_persistence)?;
    let master_key = state
        .master_key
        .as_deref()
        .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))?;
    let prices = request
        .prices
        .into_iter()
        .map(Into::into)
        .collect::<Vec<_>>();
    let revision = require_store(&state)?
        .create_pricing_revision(
            principal.user_id,
            &idempotency_key,
            request.effective_at,
            &prices,
            ReplayableIdempotency::new(request_fingerprint, master_key),
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
