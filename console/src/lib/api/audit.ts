import type { components } from './schema';
import { apiClient } from './client';
import { result } from './http';
import { type CursorPage, toCursorPage } from './pagination';
import { compactQuery } from './query';

export type AuditEvent = components['schemas']['AuditEventResponse'];

export async function listAudit(
  cursor?: string
): Promise<CursorPage<AuditEvent>> {
  const { data, error, response } = await apiClient.GET('/api/v1/audit', {
    params: { query: compactQuery({ cursor, limit: 50 }) }
  });
  return toCursorPage(result(data, error, response));
}
