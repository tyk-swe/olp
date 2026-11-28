import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/session/client.js';
import {
  unwrap,
  createHeaders,
  ifMatch,
  mutationHeaders
} from '#lib/management/http.js';
import type { ApiKey } from '#lib/domain/api-keys/api.js';

type Schemas = components['schemas'];

export type CreateApiKeyInput = Schemas['CreateApiKeyRequest'];
export type UpdateApiKeyInput = Schemas['UpdateApiKeyRequest'];
export type ApiKeyMutation = Schemas['ApiKeyMutationResponse'];
export type ApiKeySecret =
  Schemas['CreateApiKeyResponse'] | Schemas['RotateApiKeyResponse'];
export type ApiKeyPause = Schemas['ApiKeyPauseResponse'];
export type ApiKeyPauseRollout = Schemas['ApiKeyPauseRolloutResponse'];

export async function createApiKey(
  input: CreateApiKeyInput,
  requestId: string
): Promise<ApiKeySecret> {
  const response = await apiClient.POST('/api/v1/api-keys', {
    params: { header: createHeaders(requestId) },
    body: input
  });
  return unwrap(response);
}

export async function rotateApiKey(
  key: ApiKey,
  overlapSeconds: number,
  idempotencyKey: string
): Promise<ApiKeySecret> {
  const response = await apiClient.POST(
    '/api/v1/api-keys/{api_key_id}/rotate',
    {
      params: {
        path: { api_key_id: key.id },
        header: mutationHeaders(key.etag, idempotencyKey)
      },
      body: { overlap_seconds: overlapSeconds }
    }
  );
  return unwrap(response);
}

export async function updateApiKey(
  key: ApiKey,
  input: UpdateApiKeyInput
): Promise<ApiKeyMutation> {
  const response = await apiClient.PATCH('/api/v1/api-keys/{api_key_id}', {
    params: {
      path: { api_key_id: key.id },
      header: ifMatch(key.etag)
    },
    body: input
  });
  return unwrap(response);
}

export async function pauseApiKey(
  key: Pick<ApiKey, 'id' | 'etag'>,
  requestId: string
): Promise<ApiKeyPause> {
  const response = await apiClient.POST('/api/v1/api-keys/{api_key_id}/pause', {
    params: {
      path: { api_key_id: key.id },
      header: mutationHeaders(key.etag, requestId)
    }
  });
  return unwrap(response);
}

export async function resumeApiKey(
  key: Pick<ApiKey, 'id' | 'etag'>,
  requestId: string
): Promise<ApiKeyPause> {
  const response = await apiClient.POST(
    '/api/v1/api-keys/{api_key_id}/resume',
    {
      params: {
        path: { api_key_id: key.id },
        header: mutationHeaders(key.etag, requestId)
      }
    }
  );
  return unwrap(response);
}

export async function getApiKeyPauseRollout(
  apiKeyId: string,
  signal?: AbortSignal
): Promise<ApiKeyPauseRollout> {
  const response = await apiClient.GET(
    '/api/v1/api-keys/{api_key_id}/pause-rollout',
    { params: { path: { api_key_id: apiKeyId } }, signal }
  );
  return unwrap(response);
}

export async function revokeApiKey(
  key: Pick<ApiKey, 'id' | 'etag'>,
  requestId: string
): Promise<void> {
  const response = await apiClient.POST(
    '/api/v1/api-keys/{api_key_id}/revoke',
    {
      params: {
        path: { api_key_id: key.id },
        header: mutationHeaders(key.etag, requestId)
      }
    }
  );
  unwrap(response);
}
