import type { components } from '../schema';
import { apiClient } from '../client';
import { collectCursorPages, getAbortSignal, result, type CursorPage, type ReadSignal } from './shared';

type Schemas = components['schemas'];

export type ProviderKind =
  | 'openai'
  | 'anthropic'
  | 'gemini'
  | 'vertex_ai'
  | 'bedrock'
  | 'azure_openai'
  | 'openai_compatible';
export type ProviderAuthMode =
  | 'api_key'
  | 'adc'
  | 'service_account'
  | 'default_chain'
  | 'static';
export type Provider = Omit<Schemas['ProviderDetailResponse'], 'kind' | 'auth_mode'> & {
  kind: ProviderKind;
  auth_mode: ProviderAuthMode;
};
export type ProviderSummary = Schemas['ProviderSummaryResponse'];
export type ProviderModel = Schemas['ProviderModelResponse'];
export type ProviderModelInventory = Schemas['ProviderModelInventoryResponse'];
export type ProviderCredential = Schemas['CredentialResponse'] & {
  /** Credential used by the immutable runtime revision. */
  active: boolean;
  /** Credential selected only by the mutable draft. */
  draft_selected?: boolean;
};
export type CreateProviderInput = Omit<Schemas['CreateProviderRequest'], 'kind' | 'auth_mode'> & {
  kind: ProviderKind;
  auth_mode?: ProviderAuthMode | null;
};
export type UpdateProviderInput = Omit<Schemas['UpdateProviderRequest'], 'auth_mode'> & {
  auth_mode: ProviderAuthMode;
};
export type ProviderProbe = Schemas['ProbeResponse'];
export type CapabilityDeclaration = Schemas['CapabilityInput'];
export type ProviderCapabilityOptions = Omit<
  Schemas['ProviderCapabilityOptionsResponse'],
  'provider_kind'
> & { provider_kind: ProviderKind };
export type CapabilityCertification = Schemas['CapabilityCertificationResponse'];
export type ProviderRevision = Schemas['ProviderRevisionSummaryResponse'];
export type ProviderRevisionDiff = Schemas['ProviderRevisionDiffResponse'];

export async function listProviders(signal?: ReadSignal): Promise<ProviderSummary[]> {
  return collectCursorPages((cursor) => listProviderPage(cursor, getAbortSignal(signal)));
}

export async function getProviderCapabilityOptions(
  providerKind: ProviderKind,
  signal?: AbortSignal
): Promise<ProviderCapabilityOptions> {
  const response = await apiClient.GET('/api/v1/provider-kinds/{provider_kind}/capabilities', {
    params: { path: { provider_kind: providerKind } },
    signal
  });
  return result(response.data, response.error, response.response) as ProviderCapabilityOptions;
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
  const response = await apiClient.GET('/api/v1/providers/{provider_id}/models', {
    params: { path: { provider_id: providerId }, query: { limit: 50, cursor } },
    signal
  });
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
  return collectCursorPages((cursor) => listProviderModelInventoryPage(cursor, enabled, signal));
}

export async function getProvider(id: string, signal?: AbortSignal): Promise<Provider> {
  const response = await apiClient.GET('/api/v1/providers/{provider_id}', {
    params: { path: { provider_id: id } },
    signal
  });
  return result(response.data, response.error, response.response) as Provider;
}

export async function createProvider(input: CreateProviderInput): Promise<string> {
  const response = await apiClient.POST('/api/v1/providers', {
    params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
    body: input
  });
  return result(response.data, response.error, response.response).id;
}

export async function updateProvider(id: string, etag: string, input: UpdateProviderInput): Promise<Provider> {
  const response = await apiClient.PATCH('/api/v1/providers/{provider_id}', {
    params: { path: { provider_id: id }, header: { 'If-Match': etag } },
    body: input
  });
  return result(response.data, response.error, response.response) as Provider;
}

export async function probeProvider(provider: Provider): Promise<ProviderProbe> {
  const response = await apiClient.POST('/api/v1/providers/{provider_id}/probe', {
    params: {
      path: { provider_id: provider.id },
      header: { 'If-Match': provider.etag }
    } as never
  });
  return result(response.data, response.error, response.response);
}

export async function discoverProviderModels(provider: Provider): Promise<Provider> {
  const response = await apiClient.POST('/api/v1/providers/{provider_id}/discovery', {
    params: {
      path: { provider_id: provider.id },
      header: { 'If-Match': provider.etag }
    },
    body: { models: [] }
  });
  return result(response.data, response.error, response.response) as Provider;
}

/** Manual inventory fallback for compatible endpoints without a model-list API. */
export async function declareProviderModels(provider: Provider, modelNames: string[]): Promise<Provider> {
  const response = await apiClient.POST('/api/v1/providers/{provider_id}/discovery', {
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
  });
  return result(response.data, response.error, response.response) as Provider;
}

export async function setProviderModel(
  provider: Provider,
  modelId: string,
  enabled: boolean,
  capabilities: CapabilityDeclaration[]
): Promise<Provider> {
  const response = await apiClient.PATCH('/api/v1/providers/{provider_id}/models/{model_id}', {
    params: {
      path: { provider_id: provider.id, model_id: modelId },
      header: { 'If-Match': provider.etag }
    },
    body: { enabled, capabilities }
  });
  return result(response.data, response.error, response.response) as Provider;
}

export async function certifyProviderModel(
  provider: Provider,
  modelId: string
): Promise<CapabilityCertification> {
  const response = await apiClient.POST('/api/v1/providers/{provider_id}/models/{model_id}/certify', {
    params: {
      path: { provider_id: provider.id, model_id: modelId },
      header: { 'If-Match': provider.etag }
    }
  });
  return result(response.data, response.error, response.response);
}

export async function activateProvider(provider: Provider): Promise<number> {
  const response = await apiClient.POST('/api/v1/providers/{provider_id}/activate', {
    params: {
      path: { provider_id: provider.id },
      header: {
        'If-Match': provider.etag,
        'Idempotency-Key': crypto.randomUUID()
      }
    }
  });
  return result(response.data, response.error, response.response).runtime_generation.sequence;
}

export async function listProviderRevisionPage(
  providerId: string,
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<ProviderRevision>> {
  const response = await apiClient.GET('/api/v1/providers/{provider_id}/revisions', {
    params: { path: { provider_id: providerId }, query: { cursor, limit: 25 } },
    signal
  });
  const page = result(response.data, response.error, response.response);
  return { items: page.items, nextCursor: page.next_cursor ?? null };
}

export async function diffProviderRevisions(
  providerId: string,
  from: string,
  to: string,
  signal?: AbortSignal
): Promise<ProviderRevisionDiff> {
  const response = await apiClient.GET('/api/v1/providers/{provider_id}/revisions/diff', {
    params: { path: { provider_id: providerId }, query: { from, to } },
    signal
  });
  return result(response.data, response.error, response.response);
}

export async function restoreProviderRevision(
  provider: Provider,
  revisionId: string
): Promise<Provider> {
  const response = await apiClient.POST(
    '/api/v1/providers/{provider_id}/revisions/{revision_id}/restore-as-draft',
    {
      params: {
        path: { provider_id: provider.id, revision_id: revisionId },
        header: { 'If-Match': provider.etag, 'Idempotency-Key': crypto.randomUUID() }
      }
    }
  );
  return result(response.data, response.error, response.response).provider as Provider;
}

export async function listProviderCredentials(
  id: string,
  signal?: AbortSignal
): Promise<ProviderCredential[]> {
  return collectCursorPages((cursor) => listProviderCredentialPage(id, cursor, signal));
}

async function listProviderCredentialPage(
  id: string,
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<ProviderCredential>> {
  const response = await apiClient.GET('/api/v1/providers/{provider_id}/credentials', {
    params: { path: { provider_id: id }, query: { cursor, limit: 100 } },
    signal
  });
  const page = result(response.data, response.error, response.response);
  return { items: page.items, nextCursor: page.next_cursor ?? null };
}

export async function rotateProviderCredential(provider: Provider, secret: string): Promise<void> {
  const response = await apiClient.POST('/api/v1/providers/{provider_id}/credentials', {
    params: {
      path: { provider_id: provider.id },
      header: { 'If-Match': provider.etag, 'Idempotency-Key': crypto.randomUUID() }
    },
    body: { credential: secret }
  });
  result(response.data, response.error, response.response);
}

export async function revokeProviderCredential(provider: Provider, credentialId: string): Promise<void> {
  const response = await apiClient.POST('/api/v1/providers/{provider_id}/credentials/{credential_id}/revoke', {
    params: {
      path: { provider_id: provider.id, credential_id: credentialId },
      header: { 'If-Match': provider.etag, 'Idempotency-Key': crypto.randomUUID() }
    }
  });
  result(response.data, response.error, response.response);
}
