import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/session/client.js';
import { unwrap, ifMatch } from '#lib/management/http.js';
import type { User } from '#lib/domain/users/api.js';

export { listUserPage, type User } from '#lib/domain/users/api.js';

type Schemas = components['schemas'];

export type UserPatch = Schemas['UpdateUserRoleRequest'];

export async function updateUser(user: User, patch: UserPatch): Promise<User> {
  return unwrap(
    await apiClient.PATCH('/api/v1/users/{user_id}', {
      params: { path: { user_id: user.id }, header: ifMatch(user.etag) },
      body: patch
    })
  );
}
