import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { unwrap } from '$lib/api/http';
import type { Provider } from '$lib/features/providers/api/providers';

type Schemas = components['schemas'];

export type CatalogSuggestion = Schemas['ProviderCatalogSuggestion'];
export type CatalogSuggestionList =
  Schemas['ProviderCatalogSuggestionListResponse'];

/** The reference catalog's facts for a provider's models. */
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
  provider: Provider,
  catalogSHA256: string,
  upstreamModels: string[]
): Promise<Provider> {
  const response = await apiClient.POST(
    '/api/v1/providers/{provider_id}/catalog-suggestions/accept',
    {
      params: {
        path: { provider_id: provider.id },
        header: {
          'If-Match': provider.etag,
          'Idempotency-Key': crypto.randomUUID()
        }
      },
      body: { catalog_sha256: catalogSHA256, upstream_models: upstreamModels }
    }
  );
  return unwrap(response);
}
