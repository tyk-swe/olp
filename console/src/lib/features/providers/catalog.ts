import { requestNative } from '#lib/management/transport.js';
import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/management/client.js';
import { unwrap } from '#lib/management/http.js';
import type { Provider } from '#lib/features/providers/providers.js';

type Schemas = components['schemas'];

export type CatalogSuggestion = Schemas['ProviderCatalogSuggestion'];
export type CatalogSuggestionList =
  Schemas['ProviderCatalogSuggestionListResponse'];

export async function listCatalogSuggestions(
  providerId: string,
  signal?: AbortSignal
): Promise<CatalogSuggestionList> {
  const response = await apiClient.GET(
    '/api/v1/providers/{provider_id}/catalog-suggestions',
    { params: { path: { provider_id: providerId } }, signal }
  );
  return unwrap(response);
}

/**
 * Stores the catalog's facts for the named models as operator facts. The
 * digest names the catalog the operator reviewed, so an upgrade in between
 * is refused rather than storing facts nobody saw.
 */
export async function acceptCatalogSuggestions(
  provider: Pick<Provider, 'id' | 'etag'>,
  catalogSHA256: string,
  upstreamModels: string[],
  requestId: string
): Promise<Provider> {
  return requestNative(
    apiClient,
    'post',
    '/api/v1/providers/{provider_id}/catalog-suggestions/accept',
    {
      params: {
        path: { provider_id: provider.id },
        header: {
          'If-Match': provider.etag,
          'Idempotency-Key': requestId
        }
      },
      body: { catalog_sha256: catalogSHA256, upstream_models: upstreamModels }
    },
    [['configuration']]
  );
}
