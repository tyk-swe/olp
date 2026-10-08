import { apiClient } from '$lib/api/client';
import { unwrap } from '$lib/api/http';
import type { components } from '$lib/api/schema';
export type Group = components['schemas']['SCIMManagedGroup'];
export type Mapping = components['schemas']['SCIMGroupAccess'];
export async function listGroups(signal?: AbortSignal) {
  const groups: Group[] = [];
  let cursor: string | undefined;
  do {
    const page = unwrap(
      await apiClient.GET('/api/v1/scim/groups', {
        params: { query: { cursor } },
        signal
      })
    );
    groups.push(...page.items);
    cursor = page.next_cursor ?? undefined;
  } while (cursor);
  return groups;
}
export async function saveMapping(group: Group, mapping: Mapping) {
  return unwrap(
    await apiClient.PUT('/api/v1/scim/groups/{scim_id}/mapping', {
      params: {
        path: { scim_id: group.id },
        header: { 'If-Match': `"${group.etag}"` }
      },
      body: { mapping }
    })
  );
}
