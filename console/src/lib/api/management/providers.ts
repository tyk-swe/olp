import type { components } from '../schema';
import { apiClient } from '../client';
import { ApiProblem, pageResult, result } from '../http';
import { PROVIDER_PAGE_SIZE } from '../pageSizes';
import { collectCursorPages } from '../pagination';
import { type CursorPage } from '$lib/api/http';

type Schemas = components['schemas'];

export type ProviderKind = Schemas['ProviderKind'];
export type ProviderAuthMode = Schemas['ProviderAuthMode'];
export type Provider = Schemas['ProviderDetailResponse'];
export type ProviderSummary = Schemas['ProviderSummaryResponse'];

export type CreateProviderInput = Schemas['CreateProviderRequest'];
export type UpdateProviderInput = Schemas['UpdateProviderRequest'];
export type ProviderProbe = Schemas['ProbeResponse'];

export async function listProviders(
  signal?: AbortSignal
): Promise<ProviderSummary[]> {
  return collectCursorPages((cursor) => listProviderPage(cursor, signal));
}

export async function listProviderPage(
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<ProviderSummary>> {
  const response = await apiClient.GET('/api/v1/providers', {
    params: { query: { limit: PROVIDER_PAGE_SIZE, cursor } },
    signal
  });
  return pageResult(result(response.data, response.error, response.response));
}

export async function getProvider(
  id: string,
  signal?: AbortSignal
): Promise<Provider> {
  const response = await apiClient.GET('/api/v1/providers/{provider_id}', {
    params: { path: { provider_id: id } },
    signal
  });
  return result(response.data, response.error, response.response) as Provider;
}

export async function createProvider(
  input: CreateProviderInput
): Promise<string> {
  const response = await apiClient.POST('/api/v1/providers', {
    params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
    body: input
  });
  return result(response.data, response.error, response.response).id;
}

export async function updateProvider(
  id: string,
  etag: string,
  input: UpdateProviderInput
): Promise<Provider> {
  const response = await apiClient.PATCH('/api/v1/providers/{provider_id}', {
    params: { path: { provider_id: id }, header: { 'If-Match': etag } },
    body: input
  });
  return result(response.data, response.error, response.response) as Provider;
}

export async function probeProvider(
  provider: Provider
): Promise<ProviderProbe> {
  const response = await apiClient.POST(
    '/api/v1/providers/{provider_id}/probe',
    {
      params: {
        path: { provider_id: provider.id },
        header: { 'If-Match': provider.etag }
      }
    }
  );
  return result(response.data, response.error, response.response);
}

/** Manual inventory fallback for compatible endpoints without a model-list API. */

export async function activateProvider(provider: Provider): Promise<number> {
  const response = await apiClient.POST(
    '/api/v1/providers/{provider_id}/activate',
    {
      params: {
        path: { provider_id: provider.id },
        header: {
          'If-Match': provider.etag,
          'Idempotency-Key': crypto.randomUUID()
        }
      }
    }
  );
  return result(response.data, response.error, response.response)
    .runtime_generation.sequence;
}

/**
 * Stops the active revision from serving. The server refuses with 409 while a
 * route still targets one of this provider's models, or an upstream media job
 * is still live. Returns the runtime generation that no longer carries the
 * provider.
 */
export async function disableProvider(
  provider: Provider
): Promise<number | null> {
  const response = await apiClient.POST(
    '/api/v1/providers/{provider_id}/disable',
    {
      params: {
        path: { provider_id: provider.id },
        header: {
          'If-Match': provider.etag,
          'Idempotency-Key': crypto.randomUUID()
        }
      }
    }
  );
  return (
    result(response.data, response.error, response.response).runtime_generation
      ?.sequence ?? null
  );
}

/** Moves a disabled provider back to an editable draft. */
export async function restoreProviderAsDraft(
  provider: Provider
): Promise<Provider> {
  const response = await apiClient.POST(
    '/api/v1/providers/{provider_id}/restore-as-draft',
    {
      params: {
        path: { provider_id: provider.id },
        header: {
          'If-Match': provider.etag,
          'Idempotency-Key': crypto.randomUUID()
        }
      }
    }
  );
  return result(response.data, response.error, response.response) as Provider;
}

/**
 * The 409 the server raises for a resource that is still active or referenced.
 * Several unrelated conflicts share the status — a replayed or in-flight
 * `Idempotency-Key`, an unvalidated route — so the problem type, not the
 * status, is what identifies this one.
 */
const PROVIDER_IN_USE_TYPE =
  'https://openllmproxy.dev/problems/configuration_resource_in_use';

/** True for the 409 the server returns when a resource is still referenced. */
export function isProviderInUse(error: unknown): boolean {
  return (
    error instanceof ApiProblem &&
    error.problem.status === 409 &&
    error.problem.type === PROVIDER_IN_USE_TYPE
  );
}
