import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/session/client.js';
import { unwrapPage, type CursorPage } from '#lib/management/http.js';
import { collectCursorPages } from '#lib/management/pagination.js';

export type BudgetGroup = components['schemas']['BudgetGroupDetailResponse'];

export async function listBudgetGroups(
  signal?: AbortSignal
): Promise<BudgetGroup[]> {
  return collectCursorPages((cursor) => listBudgetGroupPage(cursor, signal));
}

async function listBudgetGroupPage(
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<BudgetGroup>> {
  const response = await apiClient.GET('/api/v1/budget-groups', {
    params: { query: { limit: 50, cursor } },
    signal
  });
  return unwrapPage(response);
}
