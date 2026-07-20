use axum::{Router, routing::get};
use utoipa::OpenApi;

use crate::{ApiState, HealthResponse, Problem};

#[cfg(test)]
use axum::http::{HeaderMap, HeaderValue, header};
#[cfg(test)]
use chrono::Utc;
#[cfg(test)]
use olp_domain::Surface;
#[cfg(test)]
use uuid::Uuid;

mod audit;
mod health;
mod helpers;
mod media_jobs;
mod pricing;
mod request_metadata;
mod requests;
mod runtime;
mod settings;
mod usage;

pub(crate) fn router() -> Router<ApiState> {
    Router::new()
        .route("/api/v1/requests", get(requests::list_requests))
        .route("/api/v1/requests/{request_id}", get(requests::get_request))
        .route("/api/v1/media-jobs", get(media_jobs::list_media_jobs))
        .route(
            "/api/v1/media-jobs/{job_id}",
            get(media_jobs::get_media_job),
        )
        .route(
            "/api/v1/usage/time-series",
            get(usage::series::usage_time_series),
        )
        .route("/api/v1/usage/summary", get(usage::summary::usage_summary))
        .route(
            "/api/v1/usage/breakdown",
            get(usage::breakdown::usage_breakdown),
        )
        .route(
            "/api/v1/usage/completeness",
            get(usage::completeness::usage_completeness),
        )
        .route(
            "/api/v1/request-metadata/gateway-epochs",
            get(request_metadata::list_request_metadata_gateway_epochs),
        )
        .route(
            "/api/v1/request-metadata/gateway-epochs/{process_epoch}/acknowledge",
            axum::routing::post(request_metadata::acknowledge_request_metadata_gateway_epoch),
        )
        .route("/api/v1/audit", get(audit::list_audit_events))
        .route("/api/v1/health/ready", get(health::management_readiness))
        .route("/api/v1/provider-health", get(health::provider_health))
        .route(
            "/api/v1/runtime-generations",
            get(runtime::list_runtime_generations),
        )
        .route("/api/v1/settings", get(settings::list_settings))
        .route(
            "/api/v1/settings/{key}",
            get(settings::get_setting).put(settings::update_setting),
        )
        .route(
            "/api/v1/pricing/revisions",
            get(pricing::list_pricing_revisions).post(pricing::create_pricing_revision),
        )
}

#[derive(OpenApi)]
#[openapi(
    paths(
        requests::list_requests,
        requests::get_request,
        media_jobs::list_media_jobs,
        media_jobs::get_media_job,
        usage::series::usage_time_series,
        usage::summary::usage_summary,
        usage::breakdown::usage_breakdown,
        usage::completeness::usage_completeness,
        request_metadata::list_request_metadata_gateway_epochs,
        request_metadata::acknowledge_request_metadata_gateway_epoch,
        audit::list_audit_events,
        health::management_readiness,
        health::provider_health,
        runtime::list_runtime_generations,
        settings::list_settings,
        settings::get_setting,
        settings::update_setting,
        pricing::list_pricing_revisions,
        pricing::create_pricing_revision
    ),
    components(schemas(
        requests::RequestSummary,
        requests::RequestListResponse,
        requests::AttemptResponse,
        requests::RequestDetailResponse,
        media_jobs::MediaJobItem,
        media_jobs::MediaJobListResponse,
        usage::series::UsagePointResponse,
        usage::UsageRangeCoverageResponse,
        request_metadata::RequestMetadataConsumerStatusResponse,
        usage::series::UsageTimeSeriesResponse,
        usage::summary::UsageSummaryResponse,
        usage::breakdown::UsageBreakdownItem,
        usage::breakdown::UsageBreakdownResponse,
        usage::completeness::UsageCompletenessResponse,
        request_metadata::RequestMetadataGatewayEpochListResponse,
        request_metadata::RequestMetadataGatewayEpochResponse,
        request_metadata::RequestMetadataEpochAcknowledgementResponse,
        audit::AuditEventResponse,
        audit::AuditListResponse,
        HealthResponse,
        health::ProviderHealthItem,
        health::ProviderHealthResponse,
        runtime::RuntimeGenerationItem,
        runtime::RuntimeGenerationListResponse,
        settings::SettingResponse,
        settings::SettingsResponse,
        settings::UpdateSettingRequest,
        pricing::PriceProviderKind,
        pricing::PriceOperation,
        pricing::PriceRequest,
        pricing::PricingRevisionRequest,
        pricing::PriceResponse,
        pricing::PricingRevisionResponse,
        pricing::PricingRevisionsResponse,
        Problem
    )),
    tags(
        (name = "requests"),
        (name = "media-jobs"),
        (name = "usage"),
        (name = "request-metadata"),
        (name = "audit"),
        (name = "health"),
        (name = "runtime"),
        (name = "settings"),
        (name = "pricing")
    )
)]
pub(crate) struct OperationsApiDoc;

#[cfg(test)]
use helpers::{page_limit, validate_time_range};
#[cfg(test)]
use media_jobs::media_job_surface_wire_value;
#[cfg(test)]
use settings::if_match;

#[cfg(test)]
mod tests;
