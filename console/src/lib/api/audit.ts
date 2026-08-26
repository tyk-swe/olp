import type { components } from './schema';
import { apiClient } from './client';
import { result } from './http';
import type { CursorPage } from './pagination';
import { compactQuery } from './query';

export type AuditEvent = components['schemas']['AuditEventResponse'];

/** One request fills a page; the list state and the API agree on the size. */
export const AUDIT_PAGE_SIZE = 50;

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
  const page = result(data, error, response);
  return { items: page.data, nextCursor: page.next_cursor ?? null };
}
