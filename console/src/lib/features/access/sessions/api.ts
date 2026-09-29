import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { ensureOk, unwrapPage } from '$lib/api/http';
import type { CursorPage } from '$lib/api/http';

type Schemas = components['schemas'];

export type Session = Schemas['SessionDetailResponse'];

export async function listSessionPage(
  userId?: string,
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<Session>> {
  return unwrapPage(
    await apiClient.GET('/api/v1/sessions', {
      params: { query: { limit: 50, user_id: userId, cursor } },
      signal
    })
  );
}

export async function revokeSession(id: string): Promise<void> {
  ensureOk(
    await apiClient.DELETE('/api/v1/sessions/{session_id}', {
      params: { path: { session_id: id } }
    })
  );
}
