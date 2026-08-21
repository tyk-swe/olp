import type { components } from './schema';
import { apiClient } from './client';
import { result } from './http';
import type { CursorPage } from './pagination';
import { compactQuery } from './query';

export type AuditEvent = components['schemas']['AuditEventResponse'];

export async function listAudit(
  cursor?: string
): Promise<CursorPage<AuditEvent>> {
  const { data, error, response } = await apiClient.GET('/api/v1/audit', {
    params: { query: compactQuery({ cursor, limit: 50 }) }
  });
  const page = result(data, error, response);
  return { items: page.data, nextCursor: page.next_cursor ?? null };
}
