import type { components } from '../schema';
import { apiClient } from '../client';
import { ApiProblem, result } from '../http';
import { collectCursorPages, type CursorPage } from '../pagination';

type Schemas = components['schemas'];

export type ProviderKind = Schemas['ProviderKind'];
export type ProviderAuthMode = Schemas['ProviderAuthMode'];
export type Provider = Schemas['ProviderDetailResponse'];
export type ProviderSummary = Schemas['ProviderSummaryResponse'];
export type ProviderModel = Schemas['ProviderModelResponse'];
export type ProviderModelInventory = Schemas['ProviderModelInventoryResponse'];
export type ProviderCredential = Schemas['CredentialResponse'];
export type CreateProviderInput = Schemas['CreateProviderRequest'];
export type UpdateProviderInput = Schemas['UpdateProviderRequest'];
export type ProviderProbe = Schemas['ProbeResponse'];
export type CapabilityDeclaration = Schemas['CapabilityInput'];
export type ProviderCapabilityOptions =
  Schemas['ProviderCapabilityOptionsResponse'];
export type ProviderKindCapability = Schemas['ProviderKindCapabilityResponse'];
export type ProviderPreset = Schemas['ProviderPresetResponse'];
export type CapabilityCertification =
  Schemas['CapabilityCertificationResponse'];
export type ProviderRevision = Schemas['ProviderRevisionSummaryResponse'];
export type ProviderRevisionDiff = Schemas['ProviderRevisionDiffResponse'];
export type ProviderRevisionDetail = Schemas['ProviderRevisionResponse'];
export type ProviderRevisionRestore =
  Schemas['ProviderRevisionRestoreResponse'];

export async function listProviders(
  signal?: AbortSignal
): Promise<ProviderSummary[]> {
  return collectCursorPages((cursor) => listProviderPage(cursor, signal));
}

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
  return result(
    response.data,
    response.error,
    response.response
  ) as ProviderCapabilityOptions;
}

export async function listProviderKinds(
  signal?: AbortSignal
): Promise<ProviderKindCapability[]> {
  const response = await apiClient.GET('/api/v1/provider-kinds', { signal });
  return result(response.data, response.error, response.response).items;
}

export async function listProviderPage(
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<ProviderSummary>> {
  const response = await apiClient.GET('/api/v1/providers', {
    params: { query: { limit: 50, cursor } },
    signal
  });
  const page = result(response.data, response.error, response.response);
  return { items: page.items, nextCursor: page.next_cursor ?? null };
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
        query: { limit: 50, cursor }
      },
      signal
    }
  );
  const page = result(response.data, response.error, response.response);
  return { items: page.items, nextCursor: page.next_cursor ?? null };
}

export async function listProviderModelInventoryPage(
  cursor?: string,
  enabled?: boolean,
  signal?: AbortSignal
): Promise<CursorPage<ProviderModelInventory>> {
  const response = await apiClient.GET('/api/v1/provider-models', {
    params: { query: { limit: 50, cursor, enabled } },
    signal
  });
  const page = result(response.data, response.error, response.response);
  return { items: page.items, nextCursor: page.next_cursor ?? null };
}

export async function listProviderModelInventory(
  enabled?: boolean,
  signal?: AbortSignal
): Promise<ProviderModelInventory[]> {
  return collectCursorPages((cursor) =>
    listProviderModelInventoryPage(cursor, enabled, signal)
  );
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

/** Manual inventory fallback for compatible endpoints without a model-list API. */
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

export async function listProviderRevisionPage(
  providerId: string,
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<ProviderRevision>> {
  const response = await apiClient.GET(
    '/api/v1/providers/{provider_id}/revisions',
    {
      params: {
        path: { provider_id: providerId },
        query: { cursor, limit: 25 }
      },
      signal
    }
  );
  const page = result(response.data, response.error, response.response);
  return { items: page.items, nextCursor: page.next_cursor ?? null };
}

export async function getProviderRevision(
  providerId: string,
  revisionId: string,
  signal?: AbortSignal
): Promise<ProviderRevisionDetail> {
  const response = await apiClient.GET(
    '/api/v1/providers/{provider_id}/revisions/{revision_id}',
    {
      params: {
        path: { provider_id: providerId, revision_id: revisionId }
      },
      signal
    }
  );
  return result(response.data, response.error, response.response);
}

export async function listProviderRevisionModelPage(
  providerId: string,
  revisionId: string,
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<ProviderModel>> {
  const response = await apiClient.GET(
    '/api/v1/providers/{provider_id}/revisions/{revision_id}/models',
    {
      params: {
        path: { provider_id: providerId, revision_id: revisionId },
        query: { cursor, limit: 25 }
      },
      signal
    }
  );
  const page = result(response.data, response.error, response.response);
  return { items: page.items, nextCursor: page.next_cursor ?? null };
}

export async function diffProviderRevisions(
  providerId: string,
  from: string,
  to: string,
  signal?: AbortSignal
): Promise<ProviderRevisionDiff> {
  const response = await apiClient.GET(
    '/api/v1/providers/{provider_id}/revisions/diff',
    {
      params: { path: { provider_id: providerId }, query: { from, to } },
      signal
    }
  );
  return result(response.data, response.error, response.response);
}

export async function restoreProviderRevision(
  provider: Provider,
  revisionId: string
): Promise<ProviderRevisionRestore> {
  const response = await apiClient.POST(
    '/api/v1/providers/{provider_id}/revisions/{revision_id}/restore-as-draft',
    {
      params: {
        path: { provider_id: provider.id, revision_id: revisionId },
        header: {
          'If-Match': provider.etag,
          'Idempotency-Key': crypto.randomUUID()
        }
      }
    }
  );
  return result(
    response.data,
    response.error,
    response.response
  ) as ProviderRevisionRestore;
}

export async function listProviderCredentials(
  id: string,
  signal?: AbortSignal
): Promise<ProviderCredential[]> {
  return collectCursorPages((cursor) =>
    listProviderCredentialPage(id, cursor, signal)
  );
}

async function listProviderCredentialPage(
  id: string,
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<ProviderCredential>> {
  const response = await apiClient.GET(
    '/api/v1/providers/{provider_id}/credentials',
    {
      params: { path: { provider_id: id }, query: { cursor, limit: 100 } },
      signal
    }
  );
  const page = result(response.data, response.error, response.response);
  return { items: page.items, nextCursor: page.next_cursor ?? null };
}

export async function rotateProviderCredential(
  provider: Provider,
  secret: string
): Promise<void> {
  const response = await apiClient.POST(
    '/api/v1/providers/{provider_id}/credentials',
    {
      params: {
        path: { provider_id: provider.id },
        header: {
          'If-Match': provider.etag,
          'Idempotency-Key': crypto.randomUUID()
        }
      },
      body: { credential: secret }
    }
  );
  result(response.data, response.error, response.response);
}

export async function revokeProviderCredential(
  provider: Provider,
  credentialId: string
): Promise<void> {
  const response = await apiClient.POST(
    '/api/v1/providers/{provider_id}/credentials/{credential_id}/revoke',
    {
      params: {
        path: { provider_id: provider.id, credential_id: credentialId },
        header: {
          'If-Match': provider.etag,
          'Idempotency-Key': crypto.randomUUID()
        }
      }
    }
  );
  result(response.data, response.error, response.response);
}
