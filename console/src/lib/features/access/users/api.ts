import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { unwrap, unwrapPage } from '$lib/api/http';
import type { CursorPage } from '$lib/api/http';
import { collectCursorPages } from '$lib/api/pagination';

type Schemas = components['schemas'];

export type User = Schemas['UserDetailResponse'];
export type UserPatch = Schemas['UpdateUserRoleRequest'];

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

export async function updateUser(user: User, patch: UserPatch): Promise<User> {
  return unwrap(
    await apiClient.PATCH('/api/v1/users/{user_id}', {
      params: { path: { user_id: user.id }, header: { 'If-Match': user.etag } },
      body: patch
    })
  );
}
