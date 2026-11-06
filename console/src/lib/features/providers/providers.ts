import { requestNative } from '$lib/management/transport';
import type { components } from '$lib/management/schema';
import { apiClient } from '$lib/management/client';
import { ApiProblem, ensureOk, unwrap, unwrapPage } from '$lib/management/http';
import { PROVIDER_PAGE_SIZE } from '$lib/management/pageSizes';
import { collectCursorPages } from '$lib/management/pagination';
import { type CursorPage } from '$lib/management/http';

type Schemas = components['schemas'];

export type ProviderKind = Schemas['ProviderKind'];
export type ProviderAuthMode = Schemas['ProviderAuthMode'];
export type Provider = Schemas['ProviderDetailResponse'];
export type ProviderSummary = Schemas['ProviderSummaryResponse'];

export type CreateProviderInput = Schemas['CreateProviderRequest'];
export type UpdateProviderInput = Schemas['UpdateProviderRequest'];
export type ProviderProbe = Schemas['ProbeResponse'];

export type ProviderProbeReceipt = { result: ProviderProbe; etag: string };

export type ProviderVendor = Schemas['Vendor'];

export async function listProviderVendors(
  signal?: AbortSignal
): Promise<ProviderVendor[]> {
  const response = await apiClient.GET('/api/v1/provider-vendors', { signal });
  return unwrap(response);
}

export async function listProviders(
  signal?: AbortSignal
): Promise<ProviderSummary[]> {
  return collectCursorPages((cursor) => listProviderPage(cursor, signal));
}

export async function listProviderPage(
  cursor?: string,
  signal?: AbortSignal,
  search?: string
): Promise<CursorPage<ProviderSummary>> {
  const response = await apiClient.GET('/api/v1/providers', {
    params: {
      query: { limit: PROVIDER_PAGE_SIZE, cursor, search: search || undefined }
    },
    signal
  });
  return unwrapPage(response);
}

export async function getProvider(
  id: string,
  signal?: AbortSignal
): Promise<Provider> {
  return requestNative(
    apiClient,
    'get',
    '/api/v1/providers/{provider_id}',
    {
      params: { path: { provider_id: id } },
      signal
    },
    [['configuration']]
  );
}

export async function createProvider(
  input: CreateProviderInput,
  requestId: string
): Promise<string> {
  const response = await apiClient.POST('/api/v1/providers', {
    params: { header: { 'Idempotency-Key': requestId } },
    body: input
  });
  return unwrap(response).id;
}

export async function updateProvider(
  id: string,
  etag: string,
  input: UpdateProviderInput
): Promise<Provider> {
  return requestNative(
    apiClient,
    'patch',
    '/api/v1/providers/{provider_id}',
    {
      params: { path: { provider_id: id }, header: { 'If-Match': etag } },
      body: input
    },
    [['configuration']]
  );
}

export async function probeProvider(
  provider: Provider
): Promise<ProviderProbeReceipt> {
  const response = await apiClient.POST(
    '/api/v1/providers/{provider_id}/probe',
    {
      params: {
        path: { provider_id: provider.id },
        header: { 'If-Match': provider.etag }
      }
    }
  );
  const result = unwrap(response);
  const etag = response.response.headers.get('ETag');
  if (!etag?.trim())
    throw new Error(
      'The probe response has no provider version. Refresh before continuing.'
    );
  return { result, etag };
}

export async function activateProvider(
  provider: Pick<Provider, 'id' | 'etag'>,
  requestId: string
): Promise<number> {
  const response = await apiClient.POST(
    '/api/v1/providers/{provider_id}/activate',
    {
      params: {
        path: { provider_id: provider.id },
        header: {
          'If-Match': provider.etag,
          'Idempotency-Key': requestId
        }
      }
    }
  );
  return unwrap(response).runtime_generation.sequence;
}

export async function disableProvider(
  provider: Pick<Provider, 'id' | 'etag'>,
  requestId: string
): Promise<number | null> {
  const response = await apiClient.POST(
    '/api/v1/providers/{provider_id}/disable',
    {
      params: {
        path: { provider_id: provider.id },
        header: {
          'If-Match': provider.etag,
          'Idempotency-Key': requestId
        }
      }
    }
  );
  return unwrap(response).runtime_generation?.sequence ?? null;
}

export async function restoreProviderAsDraft(
  provider: Pick<Provider, 'id' | 'etag'>,
  requestId: string
): Promise<Provider> {
  return requestNative(
    apiClient,
    'post',
    '/api/v1/providers/{provider_id}/restore-as-draft',
    {
      params: {
        path: { provider_id: provider.id },
        header: {
          'If-Match': provider.etag,
          'Idempotency-Key': requestId
        }
      }
    },
    [['configuration']]
  );
}

const PROVIDER_IN_USE_TYPE =
  'https://openllmproxy.dev/problems/configuration_resource_in_use';

export function isProviderInUse(error: unknown): boolean {
  return (
    error instanceof ApiProblem &&
    error.problem.status === 409 &&
    error.problem.type === PROVIDER_IN_USE_TYPE
  );
}

export const deleteProvider = async (
  value: { id: string; etag: string },
  requestId: string
) =>
  ensureOk(
    await apiClient.DELETE('/api/v1/providers/{provider_id}', {
      params: {
        path: { provider_id: value.id },
        header: { 'If-Match': `"${value.etag}"`, 'Idempotency-Key': requestId }
      }
    })
  );
