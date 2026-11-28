import { requestNative } from '#lib/management/transport.js';
import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/session/client.js';
import {
  ApiProblem,
  ensureOk,
  unwrap,
  createHeaders,
  ifMatch,
  mutationHeaders
} from '#lib/management/http.js';
import type { Provider } from '#lib/domain/providers/api.js';

type Schemas = components['schemas'];

export type ProviderAuthMode = Schemas['ProviderAuthMode'];

export type CreateProviderInput = Schemas['CreateProviderRequest'];
export type UpdateProviderInput = Schemas['UpdateProviderRequest'];
export type ProviderProbe = Schemas['ProbeResponse'];

export type ProviderProbeReceipt = { result: ProviderProbe; etag: string };

export async function createProvider(
  input: CreateProviderInput,
  requestId: string
): Promise<string> {
  const response = await apiClient.POST('/api/v1/providers', {
    params: { header: createHeaders(requestId) },
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
      params: { path: { provider_id: id }, header: ifMatch(etag) },
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
        header: ifMatch(provider.etag)
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
        header: mutationHeaders(provider.etag, requestId)
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
        header: mutationHeaders(provider.etag, requestId)
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
        header: mutationHeaders(provider.etag, requestId)
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

export function isRefusedProblem(error: unknown): boolean {
  return (
    error instanceof ApiProblem &&
    error.problem.status >= 400 &&
    error.problem.status < 500 &&
    ![408, 429].includes(error.problem.status)
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
        header: mutationHeaders(value.etag, requestId)
      }
    })
  );
