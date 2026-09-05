use axum::Router;
use utoipa::OpenApi;

use crate::{management::state::ManagementState, public_http::problem::Problem};

pub(crate) mod api_keys;
pub(crate) mod providers;
mod routes;

pub(super) fn router() -> Router<ManagementState> {
    Router::new()
        .merge(providers::router())
        .merge(routes::router())
        .merge(api_keys::router())
}

#[derive(OpenApi)]
#[openapi(
    paths(
        providers::create::create_provider,
        providers::create::activate_provider,
        providers::models::list_provider_kinds,
        providers::models::list_provider_kind_capabilities,
        providers::manage::list_providers,
        providers::models::list_provider_model_inventory,
        providers::manage::get_provider,
        providers::models::list_provider_models,
        providers::manage::update_provider,
        providers::manage::disable_provider,
        providers::manage::restore_provider_as_draft,
        providers::revisions::list_provider_revisions,
        providers::revisions::get_provider_revision,
        providers::revisions::list_provider_revision_models,
        providers::revisions::diff_provider_revisions,
        providers::revisions::restore_provider_revision,
        providers::credentials::list_provider_credentials,
        providers::credentials::rotate_provider_credential,
        providers::credentials::revoke_provider_credential,
        providers::manage::probe_provider,
        providers::models::discover_provider_models,
        providers::models::set_provider_model,
        providers::models::certify_provider_model,
        routes::create::create_route_draft,
        routes::create::validate_route_draft,
        routes::create::activate_route_draft,
        routes::manage::list_route_drafts,
        routes::manage::get_route_draft,
        routes::manage::replace_route_draft,
        routes::manage::delete_route_draft,
        routes::manage::simulate_route_draft,
        routes::manage::list_routes,
        routes::manage::get_route,
        routes::manage::list_route_revisions,
        routes::manage::get_route_revision,
        routes::manage::diff_route_revisions,
        routes::manage::restore_route_revision,
        api_keys::create::create_api_key,
        api_keys::create::revoke_api_key,
        api_keys::manage::list_api_keys,
        api_keys::manage::get_api_key,
        api_keys::manage::update_api_key,
        api_keys::manage::rotate_api_key
    ),
    components(schemas(
        crate::management::pagination::PageQuery,
        providers::create::CreateProviderRequest,
        providers::create::ProviderResponse,
        providers::create::ProviderActivationResponse,
        providers::models::ProviderCapabilityOptionsResponse,
        providers::models::ProviderAuthCapabilityResponse,
        providers::models::ProviderFieldCapabilityResponse,
        providers::models::ProviderPresetResponse,
        providers::models::ProviderKindCapabilityResponse,
        providers::models::ProviderKindCapabilityListResponse,
        providers::models::CapabilityResponse,
        providers::models::ProviderModelResponse,
        providers::models::ProviderModelListResponse,
        providers::models::ProviderModelInventoryResponse,
        providers::models::ProviderModelInventoryListResponse,
        providers::manage::ProviderSummaryResponse,
        providers::manage::ProviderDetailResponse,
        providers::manage::ProviderListResponse,
        providers::revisions::ProviderRevisionSummaryResponse,
        providers::revisions::ProviderRevisionResponse,
        providers::revisions::ProviderRevisionListResponse,
        providers::revisions::ProviderRevisionDiffResponse,
        providers::revisions::ProviderRevisionRestoreResponse,
        providers::manage::UpdateProviderRequest,
        providers::credentials::CredentialResponse,
        providers::credentials::CredentialListResponse,
        providers::credentials::RotateCredentialRequest,
        providers::credentials::ProviderMutationResponse,
        providers::manage::ProbeResponse,
        providers::models::CapabilityInput,
        providers::models::DiscoveredModelRequest,
        providers::models::DiscoverModelsRequest,
        providers::models::SetModelRequest,
        providers::models::CapabilityCertificationItemResponse,
        providers::models::CapabilityCertificationResponse,
        routes::create::CreateRouteDraftRequest,
        routes::create::RouteTargetRequest,
        routes::create::RouteDraftResponse,
        routes::create::RouteActivationResponse,
        routes::manage::RouteTargetResponse,
        routes::manage::RouteDraftDetailResponse,
        routes::manage::RouteDraftListResponse,
        routes::manage::ReplaceRouteDraftRequest,
        routes::manage::ReplaceRouteTargetRequest,
        routes::manage::SimulateRouteRequest,
        routes::manage::RouteSimulationTargetResponse,
        routes::manage::RouteSimulationResponse,
        routes::manage::RouteDetailResponse,
        routes::manage::RouteListResponse,
        routes::manage::RouteRevisionResponse,
        routes::manage::RouteRevisionListResponse,
        routes::manage::RouteRevisionDiffResponse,
        api_keys::manage::ApiKeyDetailResponse,
        api_keys::manage::ApiKeyBudgetResponse,
        api_keys::manage::ApiKeyBudgetWindowResponse,
        api_keys::manage::ApiKeyListResponse,
        api_keys::manage::UpdateApiKeyRequest,
        api_keys::manage::ApiKeyMutationResponse,
        api_keys::manage::RotateApiKeyRequest,
        api_keys::manage::RotateApiKeyResponse,
        api_keys::create::CreateApiKeyRequest,
        api_keys::create::CreateApiKeyResponse,
        crate::management::response_policy::RuntimeGenerationResponse,
        Problem
    )),
    tags(
        (name = "providers"),
        (name = "routes"),
        (name = "api-keys")
    )
)]
pub(super) struct ConfigurationApiDoc;

#[cfg(test)]
mod tests;
