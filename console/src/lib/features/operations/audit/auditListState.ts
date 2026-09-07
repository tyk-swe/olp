import type { AuditFilters } from '$lib/api/audit';
import { AUDIT_PAGE_SIZE } from '$lib/api/pageSizes';
import {
  filteredListState,
  type FilteredListState
} from '$lib/lists/pagination';
import { instant } from '$lib/api/query';
import { dateTimeLocalValue } from '$lib/format';

export type AuditForm = {
  action: string;
  resourceType: string;
  resourceId: string;
  actorUserId: string;
  outcome: string;
  occurredAfter: string;
  occurredBefore: string;
};

export type AuditListState = FilteredListState<
  AuditForm,
  Omit<AuditFilters, 'cursor'>
>;

export function auditRangeError(
  state: AuditForm,
  applied?: Omit<AuditFilters, 'cursor'>
): string | null {
  const query = auditFilters(state, applied);
  const after = query.occurred_after;
  const before = query.occurred_before;
  if (!after || !before || timeOrder(after) < timeOrder(before)) return null;
  return 'Occurred before must be later than occurred after.';
}

export function auditFilters(
  state: AuditForm,
  applied?: Omit<AuditFilters, 'cursor'>
): Omit<AuditFilters, 'cursor'> {
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
  toQuery: auditFilters,
  validate: auditRangeError
});

const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export function auditProblem(
  form: AuditForm,
  fromUrl = false,
  applied?: Omit<AuditFilters, 'cursor'>
): string | null {
  if (form.actorUserId.trim() && !uuid.test(form.actorUserId.trim()))
    return `Actor user ID must be a UUID: ${form.actorUserId}`;
  if (form.outcome && !['success', 'failure'].includes(form.outcome))
    return `Unknown audit outcome: ${form.outcome}`;
  for (const [value, label] of [
    [form.occurredAfter, 'Occurred after'],
    [form.occurredBefore, 'Occurred before']
  ]) {
    if (value.trim() && !auditTimeValid(value, fromUrl))
      return `${label} must be a valid time${fromUrl ? ' with a timezone' : ''}: ${value}`;
  }
  return auditRangeError(form, applied);
}

export function readAuditForm(search: URLSearchParams): AuditForm {
  return {
    action: search.get('action')?.trim() ?? '',
    resourceType: search.get('resource_type')?.trim() ?? '',
    resourceId: search.get('resource_id')?.trim() ?? '',
    actorUserId: search.get('actor_user_id')?.trim() ?? '',
    outcome: search.get('outcome')?.trim() ?? '',
    occurredAfter: search.get('occurred_after')?.trim() ?? '',
    occurredBefore: search.get('occurred_before')?.trim() ?? ''
  };
}

export function auditState(search: URLSearchParams): AuditListState {
  const form = readAuditForm(search);
  return {
    ...auditList.empty(),
    ...form,
    applied: auditFilters(form),
    occurredAfter: auditTimeValid(form.occurredAfter, true)
      ? dateTimeLocalValue(form.occurredAfter)
      : form.occurredAfter,
    occurredBefore: auditTimeValid(form.occurredBefore, true)
      ? dateTimeLocalValue(form.occurredBefore)
      : form.occurredBefore
  };
}

export function auditSearch(applied: Omit<AuditFilters, 'cursor'>): string {
  const search = new URLSearchParams();
  for (const name of [
    'action',
    'resource_type',
    'resource_id',
    'actor_user_id',
    'outcome',
    'occurred_after',
    'occurred_before'
  ] as const) {
    const value = applied[name];
    if (value !== undefined && value !== '') search.set(name, value);
  }
  return search.toString();
}

export function auditTimeValid(value: string, fromUrl = false): boolean {
  const match =
    /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2})(\.\d{1,9})?)?(Z|[+-]\d{2}:\d{2})?$/i.exec(
      value
    );
  if (!match || (fromUrl && !match[8]) || (match[8] && !match[6])) return false;
  const [year, month, day, hour, minute, second] = match
    .slice(1, 7)
    .map(Number);
  const leap = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
  const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  return (
    month >= 1 &&
    month <= 12 &&
    day >= 1 &&
    day <= days[month - 1] &&
    hour <= 23 &&
    minute <= 59 &&
    (!match[6] || second <= 59) &&
    instant(value) !== undefined
  );
}

function preservedTime(value: string, applied?: string): string | undefined {
  value = value.trim();
  if (applied && value === dateTimeLocalValue(applied)) return applied;
  if (!auditTimeValid(value)) return undefined;
  return /(?:Z|[+-]\d{2}:\d{2})$/i.test(value) ? value : instant(value);
}

function timeOrder(value: string): string {
  const fraction = /\.(\d+)/.exec(value)?.[1] ?? '';
  return `${new Date(value).toISOString().slice(0, 19)}.${fraction.padEnd(9, '0')}`;
}
