import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { pageResult, result } from '$lib/api/http';
import { AUDIT_PAGE_SIZE } from '$lib/api/pageSizes';
import type { CursorPage } from '$lib/api/http';
import { compactQuery } from '$lib/api/query';

export type AuditEvent = components['schemas']['AuditEventResponse'];

export type AuditFilters = {
  cursor?: string;
  limit?: number;
  action?: string;
  resource_type?: string;
  resource_id?: string;
  actor_user_id?: string;
  outcome?: string;
  occurred_after?: string;
  occurred_before?: string;
};

export async function listAudit(
  filters: AuditFilters = {}
): Promise<CursorPage<AuditEvent>> {
  const { data, error, response } = await apiClient.GET('/api/v3/audit', {
    params: { query: compactQuery({ limit: AUDIT_PAGE_SIZE, ...filters }) }
  });
  return pageResult(result(data, error, response));
}
