import type { AuditQuery } from '$lib/features/access/audit/auditListState';

export const auditKeys = {
  page: (applied: AuditQuery, cursor?: string) =>
    ['audit', 'page', applied, cursor ?? 'first'] as const
};
