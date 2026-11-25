import { apiClient } from '#lib/session/client.js';
import {
  ensureOk,
  unwrap,
  createHeaders,
  ifMatch
} from '#lib/management/http.js';
import type { components } from '#lib/management/schema.js';
type Schemas = components['schemas'];
export type BudgetIncrease = Schemas['BudgetIncrease'];
export type IncreaseInput = Schemas['CreateBudgetIncreaseRequest'];
export type BudgetIncreaseRequest = Schemas['BudgetIncreaseRequest'];
export type IncreaseRequestInput =
  Schemas['CreateBudgetIncreaseRequestRequest'];
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
      params: { header: createHeaders(idempotencyKey) },
      body
    })
  );
}
export async function listIncreaseRequests(
  cursor?: string,
  signal?: AbortSignal
) {
  return unwrap(
    await apiClient.GET('/api/v1/budget-increase-requests', {
      params: { query: { cursor, limit: 50 } },
      signal
    })
  );
}
export async function createIncreaseRequest(
  body: IncreaseRequestInput,
  idempotencyKey: string
) {
  return unwrap(
    await apiClient.POST('/api/v1/budget-increase-requests', {
      params: { header: createHeaders(idempotencyKey) },
      body
    })
  );
}
export async function approveIncreaseRequest(
  item: BudgetIncreaseRequest,
  idempotencyKey: string
) {
  return unwrap(
    await apiClient.POST(
      '/api/v1/budget-increase-requests/{request_id}/approve',
      {
        params: {
          path: { request_id: item.id },
          header: createHeaders(idempotencyKey)
        }
      }
    )
  );
}
export async function declineIncreaseRequest(
  item: BudgetIncreaseRequest,
  idempotencyKey: string
) {
  return unwrap(
    await apiClient.POST(
      '/api/v1/budget-increase-requests/{request_id}/decline',
      {
        params: {
          path: { request_id: item.id },
          header: createHeaders(idempotencyKey)
        }
      }
    )
  );
}
export async function revokeIncrease(item: BudgetIncrease) {
  ensureOk(
    await apiClient.DELETE('/api/v1/budget-increases/{increase_id}', {
      params: {
        path: { increase_id: item.id },
        header: ifMatch(item.etag)
      }
    })
  );
}
