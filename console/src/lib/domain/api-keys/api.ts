import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/session/client.js';
import { unwrap, unwrapPage, type CursorPage } from '#lib/management/http.js';
import { collectCursorPages } from '#lib/management/pagination.js';

export type ApiKey = components['schemas']['ApiKeyDetailResponse'];

export async function listApiKeys(signal?: AbortSignal): Promise<ApiKey[]> {
  return collectCursorPages((cursor) => listApiKeyPage(cursor, signal));
}

export async function listApiKeyPage(
  cursor?: string,
  signal?: AbortSignal,
  createdBy?: string
): Promise<CursorPage<ApiKey>> {
  const response = await apiClient.GET('/api/v1/api-keys', {
    params: { query: { limit: 50, cursor, created_by: createdBy } },
    signal
  });
  return unwrapPage(response);
}

export async function getApiKey(
  apiKeyId: string,
  signal?: AbortSignal
): Promise<ApiKey> {
  const response = await apiClient.GET('/api/v1/api-keys/{api_key_id}', {
    params: { path: { api_key_id: apiKeyId } },
    signal
  });
  return unwrap(response);
}
