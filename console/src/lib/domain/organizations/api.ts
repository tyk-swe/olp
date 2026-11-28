import { apiClient } from '#lib/session/client.js';
import { unwrapPage } from '#lib/management/http.js';
import { collectCursorPages } from '#lib/management/pagination.js';
import type { components } from '#lib/management/schema.js';

export type Organization = components['schemas']['Organization'];

export async function listOrganizations(signal?: AbortSignal) {
  return collectCursorPages(async (cursor) =>
    unwrapPage(
      await apiClient.GET('/api/v1/organizations', {
        params: { query: { cursor } },
        signal
      })
    )
  );
}
