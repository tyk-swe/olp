import type { components } from '../schema';
import { apiClient } from '../client';
import {
  collectCursorPages,
  getAbortSignal,
  requireResponseData,
  type CursorPage,
  type ReadSignal
} from './shared';

type Schemas = components['schemas'];

export type ApiKey = Schemas['ApiKeyDetailResponse'];
export type CreateApiKeyInput = Schemas['CreateApiKeyRequest'];
export type UpdateApiKeyInput = Schemas['UpdateApiKeyRequest'];
export type ApiKeyMutation = Schemas['ApiKeyMutationResponse'];
export type ApiKeySecret = Schemas['CreateApiKeyResponse'] | Schemas['RotateApiKeyResponse'];

export async function listApiKeys(signal?: ReadSignal): Promise<ApiKey[]> {
  return collectCursorPages((cursor) => listApiKeyPage(cursor, getAbortSignal(signal)));
}

export async function listApiKeyPage(
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<ApiKey>> {
  const response = await apiClient.GET('/api/v1/api-keys', {
    params: { query: { limit: 50, cursor } },
    signal
  });
  const page = requireResponseData(response.data, response.error, response.response);
  return { items: page.items, nextCursor: page.next_cursor ?? null };
}

export async function createApiKey(input: CreateApiKeyInput): Promise<ApiKeySecret> {
  const response = await apiClient.POST('/api/v1/api-keys', {
    params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
    body: input
  });
  return requireResponseData(response.data, response.error, response.response);
}

export async function rotateApiKey(key: ApiKey): Promise<ApiKeySecret> {
  const response = await apiClient.POST('/api/v1/api-keys/{api_key_id}/rotate', {
    params: {
      path: { api_key_id: key.id },
      header: { 'If-Match': key.etag, 'Idempotency-Key': crypto.randomUUID() }
    }
  });
  return requireResponseData(response.data, response.error, response.response);
}

export async function updateApiKey(key: ApiKey, input: UpdateApiKeyInput): Promise<ApiKeyMutation> {
  const response = await apiClient.PATCH('/api/v1/api-keys/{api_key_id}', {
    params: {
      path: { api_key_id: key.id },
      header: { 'If-Match': key.etag }
    },
    body: input
  });
  return requireResponseData(response.data, response.error, response.response);
}

export async function revokeApiKey(key: ApiKey): Promise<void> {
  const response = await apiClient.POST('/api/v1/api-keys/{api_key_id}/revoke', {
    params: {
      path: { api_key_id: key.id },
      header: { 'If-Match': key.etag, 'Idempotency-Key': crypto.randomUUID() }
    }
  });
  requireResponseData(response.data, response.error, response.response);
}
