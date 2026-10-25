import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { unwrap, unwrapPage } from '$lib/api/http';
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
    '/api/v1/provider-resources',
    { params: { query: compactQuery(filters) } }
  );
  return unwrapPage({ data, error, response });
}

export async function getProviderResourceContracts(signal?: AbortSignal) {
  return unwrap(
    await apiClient.GET('/api/v1/provider-resource-contracts', { signal })
  );
}

export async function requestRetainedReconciliation(id: string, hint: string) {
  return unwrap(
    await apiClient.POST('/api/v1/provider-resources/{id}/reconcile', {
      params: { path: { id } },
      body: hint.trim() ? { upstream_id: hint.trim() } : {}
    })
  );
}

export async function submitRetainedEvidence(id: string, serialized: string) {
  return unwrap(
    await apiClient.POST('/api/v1/provider-resources/{id}/usage-evidence', {
      params: { path: { id } },
      body: {} as components['schemas']['RetainedUsageEvidenceRequest'],
      bodySerializer: () => serialized
    })
  );
}
