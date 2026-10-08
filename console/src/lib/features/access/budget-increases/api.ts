import { apiClient } from '$lib/api/client';
import { ensureOk, unwrap } from '$lib/api/http';
import type { components } from '$lib/api/schema';
type Schemas = components['schemas'];
export type BudgetIncrease = Schemas['BudgetIncrease'];
export type IncreaseInput = Schemas['CreateBudgetIncreaseRequest'];
export async function listIncreases(cursor?: string, signal?: AbortSignal) {
  return unwrap(
    await apiClient.GET('/api/v1/budget-increases', {
      params: { query: { cursor, limit: 50 } },
      signal
    })
  );
}
export async function createIncrease(
  body: IncreaseInput,
  idempotencyKey: string
) {
  return unwrap(
    await apiClient.POST('/api/v1/budget-increases', {
      params: { header: { 'Idempotency-Key': idempotencyKey } },
      body
    })
  );
}
export async function revokeIncrease(item: BudgetIncrease) {
  ensureOk(
    await apiClient.DELETE('/api/v1/budget-increases/{increase_id}', {
      params: {
        path: { increase_id: item.id },
        header: { 'If-Match': `"${item.etag}"` }
      }
    })
  );
}
