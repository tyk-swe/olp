import { createContext } from 'svelte';
import type { AuditFilters } from '$lib/api/audit';
import { AUDIT_PAGE_SIZE } from '$lib/api/pageSizes';
import { emptyCursorHistory, type CursorHistory } from '$lib/api/pagination';
import { instant } from '$lib/api/query';

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

export const [getAuditListState, setAuditListState] =
  createContext<AuditListState>();

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
 * The API rejects an inverted window with a field-validation problem, but the
 * operator can be told before the round trip. Equal bounds are a legitimate
 * point-in-time query and stay allowed.
 */
export function auditRangeError(state: AuditListState): string | null {
  const after = instant(state.occurredAfter);
  const before = instant(state.occurredBefore);
  if (!after || !before || after < before) return null;
  return 'Occurred before must be later than occurred after.';
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
