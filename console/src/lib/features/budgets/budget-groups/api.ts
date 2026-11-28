import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/session/client.js';
import {
  ensureOk,
  unwrap,
  createHeaders,
  ifMatch,
  mutationHeaders
} from '#lib/management/http.js';
import type { BudgetGroup } from '#lib/domain/budget-groups/api.js';

type Schemas = components['schemas'];

export type CreateBudgetGroupInput = Schemas['CreateBudgetGroupRequest'];
export type UpdateBudgetGroupInput = Schemas['UpdateBudgetGroupRequest'];

export async function createBudgetGroup(
  input: CreateBudgetGroupInput,
  requestId: string
): Promise<{ id: string; etag: string }> {
  const response = await apiClient.POST('/api/v1/budget-groups', {
    params: { header: createHeaders(requestId) },
    body: input
  });
  return unwrap(response);
}

export async function updateBudgetGroup(
  group: BudgetGroup,
  input: UpdateBudgetGroupInput
): Promise<{ etag: string }> {
  const response = await apiClient.PATCH(
    '/api/v1/budget-groups/{budget_group_id}',
    {
      params: {
        path: { budget_group_id: group.id },
        header: ifMatch(group.etag)
      },
      body: input
    }
  );
  return unwrap(response);
}

export const retireBudgetGroup = async (
  value: { id: string; etag: string },
  requestId: string
) =>
  ensureOk(
    await apiClient.DELETE('/api/v1/budget-groups/{budget_group_id}', {
      params: {
        path: { budget_group_id: value.id },
        header: mutationHeaders(value.etag, requestId)
      }
    })
  );
