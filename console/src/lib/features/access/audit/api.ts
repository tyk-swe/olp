import type { components } from '$lib/management/schema';
import { apiClient } from '$lib/management/client';
import { unwrapPage } from '$lib/management/http';
import { AUDIT_PAGE_SIZE } from '$lib/management/pageSizes';
import type { CursorPage } from '$lib/management/http';
import { compactQuery } from '$lib/management/query';

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
  const { data, error, response } = await apiClient.GET('/api/v1/audit', {
    params: { query: compactQuery({ limit: AUDIT_PAGE_SIZE, ...filters }) }
  });
  return unwrapPage({ data, error, response });
}
