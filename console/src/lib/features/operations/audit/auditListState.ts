import type { AuditFilters } from '$lib/api/audit';
import { emptyCursorHistory, type CursorHistory } from '$lib/api/pagination';

export type AuditListState = CursorHistory & {
  action: string;
  resourceType: string;
  resourceId: string;
  actorUserId: string;
  outcome: string;
  occurredAfter: string;
  occurredBefore: string;
  applied: Omit<AuditFilters, 'cursor'>;
};

export const AUDIT_PAGE_SIZE = 50;

export function emptyAuditListState(): AuditListState {
  return {
    ...emptyCursorHistory(),
    action: '',
    resourceType: '',
    resourceId: '',
    actorUserId: '',
    outcome: '',
    occurredAfter: '',
    occurredBefore: '',
    applied: { limit: AUDIT_PAGE_SIZE }
  };
}

/**
 * The date inputs are local-time `datetime-local` values; the API compares
 * instants. A half-typed date stays out of the query rather than being sent as
 * an invalid bound the backend would reject.
 */
function instant(value: string): string | undefined {
  if (!value.trim()) return undefined;
  const date = new Date(value);
  return Number.isNaN(date.valueOf()) ? undefined : date.toISOString();
}

export function auditFilters(
  state: AuditListState
): Omit<AuditFilters, 'cursor'> {
  return {
    limit: AUDIT_PAGE_SIZE,
    action: state.action.trim() || undefined,
    resource_type: state.resourceType.trim() || undefined,
    resource_id: state.resourceId.trim() || undefined,
    actor_user_id: state.actorUserId.trim() || undefined,
    outcome: state.outcome || undefined,
    occurred_after: instant(state.occurredAfter),
    occurred_before: instant(state.occurredBefore)
  };
}
