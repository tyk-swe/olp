import { apiClient } from '#lib/session/client.js';
import { unwrap, unwrapPage, ifMatch } from '#lib/management/http.js';
import { collectCursorPages } from '#lib/management/pagination.js';
import type { components } from '#lib/management/schema.js';
export type Group = components['schemas']['SCIMManagedGroup'];
export type Mapping = components['schemas']['SCIMGroupAccess'];
export async function listGroups(signal?: AbortSignal) {
  return collectCursorPages(async (cursor) =>
    unwrapPage(
      await apiClient.GET('/api/v1/scim/groups', {
        params: { query: { cursor } },
        signal
      })
    )
  );
}
export async function saveMapping(group: Group, mapping: Mapping) {
  return unwrap(
    await apiClient.PUT('/api/v1/scim/groups/{scim_id}/mapping', {
      params: {
        path: { scim_id: group.id },
        header: ifMatch(group.etag)
      },
      body: { mapping }
    })
  );
}
