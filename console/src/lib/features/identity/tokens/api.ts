import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/session/client.js';
import {
  ensureOk,
  unwrap,
  unwrapPage,
  createHeaders,
  mutationHeaders
} from '#lib/management/http.js';
import type { CursorPage } from '#lib/management/http.js';

type Schemas = components['schemas'];

export type ManagementToken = Schemas['ManagementTokenResponse'];
export type ManagementTokenSecret = Schemas['CreateManagementTokenResponse'];

export type ManagementTokenScope = Schemas['ManagementTokenScope'];

export async function listManagementTokenPage(
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<ManagementToken>> {
  return unwrapPage(
    await apiClient.GET('/api/v1/management-tokens', {
      params: { query: { limit: 50, cursor } },
      signal
    })
  );
}

export async function createManagementToken(
  name: string,
  scopes: ManagementTokenScope[],
  expiresAt: string,
  projectIds: string[] | undefined,
  requestId: string
): Promise<ManagementTokenSecret> {
  return unwrap(
    await apiClient.POST('/api/v1/management-tokens', {
      params: { header: createHeaders(requestId) },
      body: {
        name,
        scopes,
        expires_at: expiresAt,
        ...(projectIds ? { project_ids: projectIds } : {})
      }
    })
  );
}

export async function revokeManagementToken(
  id: string,
  etag: string,
  requestId: string
): Promise<void> {
  ensureOk(
    await apiClient.POST(
      '/api/v1/management-tokens/{management_token_id}/revoke',
      {
        params: {
          path: { management_token_id: id },
          header: mutationHeaders(etag, requestId)
        }
      }
    )
  );
}
