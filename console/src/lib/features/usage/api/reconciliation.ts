import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { unwrap } from '$lib/api/http';

type Schemas = components['schemas'];

export type ReconciliationReport = Schemas['ReconciliationReport'];
export type ReconciliationItem = ReconciliationReport['items'][number];
export type ReconciliationTotals = ReconciliationReport['totals'][number];

/** Invoiced-vs-attributed report over [start, end) UTC days; ≤ 93 days. */
export async function getReconciliation(
  start: string,
  end: string,
  providerId?: string,
  signal?: AbortSignal
): Promise<ReconciliationReport> {
  const response = await apiClient.GET('/api/v1/usage/reconciliation', {
    params: {
      query: {
        start,
        end,
        ...(providerId ? { provider_id: providerId } : {})
      }
    },
    signal
  });
  return unwrap(response);
}
