import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { pageResult, result } from '$lib/api/http';
import type { CursorPage } from '$lib/api/http';
import { compactQuery } from '$lib/api/query';

export type ProviderResource = components['schemas']['ProviderResourceItem'];

export type ProviderResourceFilters = {
  cursor?: string;
  limit?: number;
  kind?: ProviderResource['kind'];
  api_key_id?: string;
  provider_id?: string;
  route?: string;
  state?: string;
};

export async function listProviderResources(
  filters: ProviderResourceFilters
): Promise<CursorPage<ProviderResource>> {
  const { data, error, response } = await apiClient.GET(
    '/api/v3/provider-resources',
    { params: { query: compactQuery(filters) } }
  );
  return pageResult(result(data, error, response));
}
