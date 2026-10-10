import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { ensureOk, unwrap, unwrapPage } from '$lib/api/http';
import type { CursorPage } from '$lib/api/http';

type Schemas = components['schemas'];

export type ManagementToken = Schemas['ManagementTokenResponse'];
export type ManagementTokenSecret = Schemas['CreateManagementTokenResponse'];

export type ManagementTokenScope = Schemas['ManagementTokenScope'];

// Every ManagementTokenScope, in the order the token form presents them.
export const MANAGEMENT_TOKEN_SCOPES = [
  'read',
  'access_read',
  'access',
  'settings',
  'configure',
  'keys',
  'manage_organization',
  'manage_projects',
  'playground',
  'usage'
] as const satisfies readonly ManagementTokenScope[];

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
  projectIds?: string[]
): Promise<ManagementTokenSecret> {
  return unwrap(
    await apiClient.POST('/api/v1/management-tokens', {
      params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
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
  etag: string
): Promise<void> {
  ensureOk(
    await apiClient.POST(
      '/api/v1/management-tokens/{management_token_id}/revoke',
      {
        params: {
          path: { management_token_id: id },
          header: {
            'If-Match': etag,
            'Idempotency-Key': crypto.randomUUID()
          }
        }
      }
    )
  );
}
