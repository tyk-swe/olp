import type { components } from '../schema';
import { apiClient } from '../client';
import { pageResult, result, type CursorPage } from '../http';
import { PROVIDER_PAGE_SIZE } from '../pageSizes';
import { collectCursorPages } from '../pagination';
import type { Provider, ProviderKind } from './providers';

type Schemas = components['schemas'];

export type ProviderModel = Schemas['ProviderModelResponse'];

export type ProviderModelInventory = Schemas['ProviderModelInventoryResponse'];

export type CapabilityDeclaration = Schemas['CapabilityInput'];

export type ProviderCapabilityOptions =
  Schemas['ProviderCapabilityOptionsResponse'];

export type ProviderKindCapability = Schemas['ProviderKindCapabilityResponse'];

export type ProviderPreset = Schemas['ProviderPresetResponse'];

export type CapabilityCertification =
  Schemas['CapabilityCertificationResponse'];

export async function getProviderCapabilityOptions(
  providerKind: ProviderKind,
  signal?: AbortSignal
): Promise<ProviderCapabilityOptions> {
  const response = await apiClient.GET(
    '/api/v1/provider-kinds/{provider_kind}/capabilities',
    {
      params: { path: { provider_kind: providerKind } },
      signal
    }
  );
  return result(response.data, response.error, response.response);
}

export async function listProviderKinds(
  signal?: AbortSignal
): Promise<ProviderKindCapability[]> {
  const response = await apiClient.GET('/api/v1/provider-kinds', { signal });
  return result(response.data, response.error, response.response).items;
}

export async function listProviderModelPage(
  providerId: string,
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<ProviderModel>> {
  const response = await apiClient.GET(
    '/api/v1/providers/{provider_id}/models',
    {
      params: {
        path: { provider_id: providerId },
        query: { limit: PROVIDER_PAGE_SIZE, cursor }
      },
      signal
    }
  );
  return pageResult(result(response.data, response.error, response.response));
}

export async function listProviderModelInventoryPage(
  cursor?: string,
  enabled?: boolean,
  signal?: AbortSignal
): Promise<CursorPage<ProviderModelInventory>> {
  const response = await apiClient.GET('/api/v1/provider-models', {
    params: { query: { limit: PROVIDER_PAGE_SIZE, cursor, enabled } },
    signal
  });
  return pageResult(result(response.data, response.error, response.response));
}

export async function listProviderModelInventory(
  enabled?: boolean,
  signal?: AbortSignal
): Promise<ProviderModelInventory[]> {
  return collectCursorPages((cursor) =>
    listProviderModelInventoryPage(cursor, enabled, signal)
  );
}

export async function discoverProviderModels(
  provider: Provider
): Promise<Provider> {
  const response = await apiClient.POST(
    '/api/v1/providers/{provider_id}/discovery',
    {
      params: {
        path: { provider_id: provider.id },
        header: { 'If-Match': provider.etag }
      },
      body: { models: [] }
    }
  );
  return result(response.data, response.error, response.response) as Provider;
}

export async function declareProviderModels(
  provider: Provider,
  modelNames: string[]
): Promise<Provider> {
  const response = await apiClient.POST(
    '/api/v1/providers/{provider_id}/discovery',
    {
      params: {
        path: { provider_id: provider.id },
        header: { 'If-Match': provider.etag }
      },
      body: {
        models: modelNames.map((model) => ({
          upstream_model: model,
          display_name: model,
          enabled: false,
          capabilities: []
        }))
      }
    }
  );
  return result(response.data, response.error, response.response) as Provider;
}

export async function setProviderModel(
  provider: Provider,
  modelId: string,
  enabled: boolean,
  capabilities: CapabilityDeclaration[]
): Promise<Provider> {
  const response = await apiClient.PATCH(
    '/api/v1/providers/{provider_id}/models/{model_id}',
    {
      params: {
        path: { provider_id: provider.id, model_id: modelId },
        header: { 'If-Match': provider.etag }
      },
      body: { enabled, capabilities }
    }
  );
  return result(response.data, response.error, response.response) as Provider;
}

export async function certifyProviderModel(
  provider: Provider,
  modelId: string
): Promise<CapabilityCertification> {
  const response = await apiClient.POST(
    '/api/v1/providers/{provider_id}/models/{model_id}/certify',
    {
      params: {
        path: { provider_id: provider.id, model_id: modelId },
        header: { 'If-Match': provider.etag }
      }
    }
  );
  return result(response.data, response.error, response.response);
}
