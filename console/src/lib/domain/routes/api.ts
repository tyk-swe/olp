import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/session/client.js';
import { unwrap, unwrapPage, type CursorPage } from '#lib/management/http.js';
import { ROUTE_PAGE_SIZE } from '#lib/management/pageSizes.js';
import { collectCursorPages } from '#lib/management/pagination.js';

export type ActiveRoute = components['schemas']['RouteDetailResponse'];

export async function listRoutes(signal?: AbortSignal): Promise<ActiveRoute[]> {
  return collectCursorPages((cursor) => listRoutePage(cursor, signal));
}

export async function listRoutePage(
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<ActiveRoute>> {
  const response = await apiClient.GET('/api/v1/routes', {
    params: { query: { limit: ROUTE_PAGE_SIZE, cursor } },
    signal
  });
  return unwrapPage(response);
}

export const listOperationDialects = async (signal?: AbortSignal) =>
  unwrap(await apiClient.GET('/api/v1/operation-dialects', { signal }));
