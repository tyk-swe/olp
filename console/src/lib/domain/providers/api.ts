import type { components } from '#lib/management/schema.js';
import { requestNative } from '#lib/management/transport.js';
import { apiClient } from '#lib/session/client.js';
import { unwrap, unwrapPage, type CursorPage } from '#lib/management/http.js';
import {
  PROVIDER_CREDENTIAL_PAGE_SIZE,
  PROVIDER_PAGE_SIZE
} from '#lib/management/pageSizes.js';
import { collectCursorPages } from '#lib/management/pagination.js';

type Schemas = components['schemas'];

export type ProviderKind = Schemas['ProviderKind'];
export type ProviderKindCapability = Schemas['ProviderKindCapabilityResponse'];
export type ProviderVendor = Schemas['Vendor'];
export type Provider = Schemas['ProviderDetailResponse'];
export type ProviderSummary = Schemas['ProviderSummaryResponse'];
export type ProviderCredential = Schemas['CredentialResponse'];
export type CredentialSlot = Schemas['CredentialSlot'];
export type CredentialSlotPool = Schemas['SlotList'];

export async function listProviderKinds(
  signal?: AbortSignal
): Promise<ProviderKindCapability[]> {
  const response = await apiClient.GET('/api/v1/provider-kinds', { signal });
  return unwrap(response).items;
}

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
      params: {
        path: { provider_id: id },
        query: { cursor, limit: PROVIDER_CREDENTIAL_PAGE_SIZE }
      },
      signal
    }
  );
  return unwrapPage(response);
}

export async function listCredentialSlots(
  providerId: string,
  signal?: AbortSignal
): Promise<CredentialSlotPool> {
  return unwrap(
    await apiClient.GET('/api/v1/providers/{provider_id}/credential-slots', {
      params: { path: { provider_id: providerId } },
      signal
    })
  );
}
