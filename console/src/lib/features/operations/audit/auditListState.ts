import type { AuditFilters } from '$lib/api/audit';
import { AUDIT_PAGE_SIZE } from '$lib/api/pageSizes';
import {
  filteredListState,
  type FilteredListState
} from '$lib/lists/pagination';
import {
  formTimeValue,
  preservedTime,
  querySearch,
  rangeProblem,
  readForm,
  timeProblem,
  uuidProblem
} from '$lib/lists/filters';
import type { ListUrlSpec } from '$lib/lists/urlSync.svelte';

export type AuditForm = {
  action: string;
  resourceType: string;
  resourceId: string;
  actorUserId: string;
  outcome: string;
  occurredAfter: string;
  occurredBefore: string;
};

export type AuditQuery = Omit<AuditFilters, 'cursor'>;
export type AuditListState = FilteredListState<AuditForm, AuditQuery>;

const params = {
  action: 'action',
  resourceType: 'resource_type',
  resourceId: 'resource_id',
  actorUserId: 'actor_user_id',
  outcome: 'outcome',
  occurredAfter: 'occurred_after',
  occurredBefore: 'occurred_before'
} as const;

export const AUDIT_RANGE_MESSAGE =
  'Occurred before must be later than occurred after.';

export function auditRangeError(
  state: AuditForm,
  applied?: AuditQuery
): string | null {
  const query = auditFilters(state, applied);
  return rangeProblem(
    query.occurred_after,
    query.occurred_before,
    AUDIT_RANGE_MESSAGE
  );
}

export function auditFilters(
  state: AuditForm,
  applied?: AuditQuery
): AuditQuery {
  return {
    limit: AUDIT_PAGE_SIZE,
    action: state.action.trim() || undefined,
    resource_type: state.resourceType.trim() || undefined,
    resource_id: state.resourceId.trim() || undefined,
    actor_user_id: state.actorUserId.trim() || undefined,
    outcome: state.outcome || undefined,
    occurred_after: preservedTime(state.occurredAfter, applied?.occurred_after),
    occurred_before: preservedTime(
      state.occurredBefore,
      applied?.occurred_before
    )
  };
}

export const auditList = filteredListState({
  emptyForm: (): AuditForm => ({
    action: '',
    resourceType: '',
    resourceId: '',
    actorUserId: '',
    outcome: '',
    occurredAfter: '',
    occurredBefore: ''
  }),
  toQuery: auditFilters
});

export function auditProblem(
  form: AuditForm,
  fromUrl = false,
  applied?: AuditQuery
): string | null {
  const actor = uuidProblem([[form.actorUserId, 'Actor user ID']]);
  if (actor) return actor;
  if (form.outcome && !['success', 'failure'].includes(form.outcome))
    return `Unknown audit outcome: ${form.outcome}`;
  return (
    timeProblem(
      [
        [form.occurredAfter, 'Occurred after'],
        [form.occurredBefore, 'Occurred before']
      ],
      fromUrl
    ) ?? auditRangeError(form, applied)
  );
}

export function readAuditForm(search: URLSearchParams): AuditForm {
  return readForm<AuditForm>(search, params);
}

export function auditState(search: URLSearchParams): AuditListState {
  const form = readAuditForm(search);
  return {
    ...auditList.empty(),
    ...form,
    applied: auditFilters(form),
    occurredAfter: formTimeValue(form.occurredAfter),
    occurredBefore: formTimeValue(form.occurredBefore)
  };
}

export function auditSearch(applied: AuditQuery): string {
  return querySearch(applied, Object.values(params));
}

export const auditUrl: ListUrlSpec<AuditListState> = {
  path: '/audit',
  state: auditState,
  canonical: (search) => {
    const form = readAuditForm(search);
    return auditProblem(form, true) ? null : auditSearch(auditFilters(form));
  }
};
