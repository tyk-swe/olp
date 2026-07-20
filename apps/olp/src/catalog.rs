use axum::{
    Router,
    routing::{get, patch, post},
};
use utoipa::OpenApi;

use crate::{ApiState, Problem};

mod api_keys;
mod common;
mod models;
mod providers;
mod revisions;
mod routes;

pub fn router() -> Router<ApiState> {
    Router::new()
        .route(
            "/api/v1/provider-kinds/{provider_kind}/capabilities",
            get(models::list_provider_kind_capabilities),
        )
        .route("/api/v1/providers", get(providers::list_providers))
        .route(
            "/api/v1/provider-models",
            get(models::list_provider_model_inventory),
        )
        .route(
            "/api/v1/providers/{provider_id}",
            get(providers::get_provider).patch(providers::update_provider),
        )
        .route(
            "/api/v1/providers/{provider_id}/disable",
            post(providers::disable_provider),
        )
        .route(
            "/api/v1/providers/{provider_id}/restore-as-draft",
            post(providers::restore_provider_as_draft),
        )
        .route(
            "/api/v1/providers/{provider_id}/revisions",
            get(revisions::list_provider_revisions),
        )
        .route(
            "/api/v1/providers/{provider_id}/revisions/diff",
            get(revisions::diff_provider_revisions),
        )
        .route(
            "/api/v1/providers/{provider_id}/revisions/{revision_id}",
            get(revisions::get_provider_revision),
        )
        .route(
            "/api/v1/providers/{provider_id}/revisions/{revision_id}/models",
            get(revisions::list_provider_revision_models),
        )
        .route(
            "/api/v1/providers/{provider_id}/revisions/{revision_id}/restore-as-draft",
            post(revisions::restore_provider_revision),
        )
        .route(
            "/api/v1/providers/{provider_id}/credentials",
            get(providers::list_provider_credentials).post(providers::rotate_provider_credential),
        )
        .route(
            "/api/v1/providers/{provider_id}/credentials/{credential_id}/revoke",
            post(providers::revoke_provider_credential),
        )
        .route(
            "/api/v1/providers/{provider_id}/probe",
            post(providers::probe_provider),
        )
        .route(
            "/api/v1/providers/{provider_id}/discovery",
            post(models::discover_provider_models),
        )
        .route(
            "/api/v1/providers/{provider_id}/models/{model_id}",
            patch(models::set_provider_model),
        )
        .route(
            "/api/v1/providers/{provider_id}/models",
            get(models::list_provider_models),
        )
        .route(
            "/api/v1/providers/{provider_id}/models/{model_id}/certify",
            post(models::certify_provider_model),
        )
        .route("/api/v1/route-drafts", get(routes::list_route_drafts))
        .route(
            "/api/v1/route-drafts/{draft_id}",
            get(routes::get_route_draft)
                .put(routes::replace_route_draft)
                .delete(routes::delete_route_draft),
        )
        .route(
            "/api/v1/route-drafts/{draft_id}/simulate",
            post(routes::simulate_route_draft),
        )
        .route("/api/v1/routes", get(routes::list_routes))
        .route("/api/v1/routes/{route_id}", get(routes::get_route))
        .route(
            "/api/v1/routes/{route_id}/revisions",
            get(routes::list_route_revisions),
        )
        .route(
            "/api/v1/routes/{route_id}/revisions/diff",
            get(routes::diff_route_revisions),
        )
        .route(
            "/api/v1/routes/{route_id}/revisions/{revision_id}",
            get(routes::get_route_revision),
        )
        .route(
            "/api/v1/routes/{route_id}/revisions/{revision_id}/restore-as-draft",
            post(routes::restore_route_revision),
        )
        .route("/api/v1/api-keys", get(api_keys::list_api_keys))
        .route(
            "/api/v1/api-keys/{api_key_id}",
            get(api_keys::get_api_key).patch(api_keys::update_api_key),
        )
        .route(
            "/api/v1/api-keys/{api_key_id}/rotate",
            post(api_keys::rotate_api_key),
        )
}

#[derive(OpenApi)]
#[openapi(
    paths(
        models::list_provider_kind_capabilities,
        providers::list_providers,
        models::list_provider_model_inventory,
        providers::get_provider,
        models::list_provider_models,
        providers::update_provider,
        providers::disable_provider,
        providers::restore_provider_as_draft,
        revisions::list_provider_revisions,
        revisions::get_provider_revision,
        revisions::list_provider_revision_models,
        revisions::diff_provider_revisions,
        revisions::restore_provider_revision,
        providers::list_provider_credentials,
        providers::rotate_provider_credential,
        providers::revoke_provider_credential,
        providers::probe_provider,
        models::discover_provider_models,
        models::set_provider_model,
        models::certify_provider_model,
        routes::list_route_drafts,
        routes::get_route_draft,
        routes::replace_route_draft,
        routes::delete_route_draft,
        routes::simulate_route_draft,
        routes::list_routes,
        routes::get_route,
        routes::list_route_revisions,
        routes::get_route_revision,
        routes::diff_route_revisions,
        routes::restore_route_revision,
        api_keys::list_api_keys,
        api_keys::get_api_key,
        api_keys::update_api_key,
        api_keys::rotate_api_key
    ),
    components(schemas(
        common::PageQuery,
        models::ProviderCapabilityOptionsResponse,
        models::CapabilityResponse,
        models::ProviderModelResponse,
        models::ProviderModelListResponse,
        models::ProviderModelInventoryResponse,
        models::ProviderModelInventoryListResponse,
        providers::ProviderSummaryResponse,
        providers::ProviderCatalogResponse,
        providers::ProviderListResponse,
        revisions::ProviderRevisionSummaryResponse,
        revisions::ProviderRevisionResponse,
        revisions::ProviderRevisionListResponse,
        revisions::ProviderRevisionDiffResponse,
        revisions::ProviderRevisionRestoreResponse,
        providers::UpdateProviderRequest,
        providers::CredentialResponse,
        providers::CredentialListResponse,
        providers::RotateCredentialRequest,
        providers::ProviderMutationResponse,
        providers::ProbeResponse,
        models::CapabilityInput,
        models::DiscoveredModelRequest,
        models::DiscoverModelsRequest,
        models::SetModelRequest,
        models::CapabilityCertificationItemResponse,
        models::CapabilityCertificationResponse,
        routes::RouteTargetCatalogResponse,
        routes::RouteDraftCatalogResponse,
        routes::RouteDraftListResponse,
        routes::ReplaceRouteDraftRequest,
        routes::ReplaceRouteTargetRequest,
        routes::SimulateRouteRequest,
        routes::RouteSimulationTargetResponse,
        routes::RouteSimulationResponse,
        routes::RouteCatalogResponse,
        routes::RouteListResponse,
        routes::RouteRevisionResponse,
        routes::RouteRevisionListResponse,
        routes::RouteRevisionDiffResponse,
        api_keys::ApiKeyCatalogResponse,
        api_keys::ApiKeyListResponse,
        api_keys::UpdateApiKeyRequest,
        api_keys::ApiKeyMutationResponse,
        api_keys::RotateApiKeyResponse,
        common::RuntimeGenerationCatalogResponse,
        Problem
    )),
    tags(
        (name = "providers"),
        (name = "routes"),
        (name = "api-keys")
    )
)]
pub struct CatalogApiDoc;

#[cfg(test)]
mod tests;
