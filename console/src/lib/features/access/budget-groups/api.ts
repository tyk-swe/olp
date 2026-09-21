import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { pageResult, result } from '$lib/api/http';
import { type CursorPage } from '$lib/api/http';
import { collectCursorPages } from '$lib/api/pagination';

type Schemas = components['schemas'];

export type BudgetGroup = Schemas['BudgetGroupDetailResponse'];
export type CreateBudgetGroupInput = Schemas['CreateBudgetGroupRequest'];
export type UpdateBudgetGroupInput = Schemas['UpdateBudgetGroupRequest'];

export async function listBudgetGroups(
  signal?: AbortSignal
): Promise<BudgetGroup[]> {
  return collectCursorPages((cursor) => listBudgetGroupPage(cursor, signal));
}

export async function listBudgetGroupPage(
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<BudgetGroup>> {
  const response = await apiClient.GET('/api/v3/budget-groups', {
    params: { query: { limit: 50, cursor } },
    signal
  });
  return pageResult(result(response.data, response.error, response.response));
}

export async function createBudgetGroup(
  input: CreateBudgetGroupInput
): Promise<{ id: string; etag: string }> {
  const response = await apiClient.POST('/api/v3/budget-groups', {
    params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
    body: input
  });
  return result(response.data, response.error, response.response);
}

export async function updateBudgetGroup(
  group: BudgetGroup,
  input: UpdateBudgetGroupInput
): Promise<{ etag: string }> {
  const response = await apiClient.PATCH(
    '/api/v3/budget-groups/{budget_group_id}',
    {
      params: {
        path: { budget_group_id: group.id },
        header: { 'If-Match': group.etag }
      },
      body: input
    }
  );
  return result(response.data, response.error, response.response);
}
