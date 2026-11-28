import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/session/client.js';
import { unwrapPage, type CursorPage } from '#lib/management/http.js';
import { collectCursorPages } from '#lib/management/pagination.js';

export type User = components['schemas']['UserDetailResponse'];

export async function listUserPage(
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<User>> {
  return unwrapPage(
    await apiClient.GET('/api/v1/users', {
      params: { query: { limit: 50, cursor } },
      signal
    })
  );
}

export async function listUsers(signal?: AbortSignal): Promise<User[]> {
  return collectCursorPages((cursor) => listUserPage(cursor, signal));
}
