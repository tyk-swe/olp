import type { MediaJobFilters } from '$lib/api/media-jobs';
import { MEDIA_JOB_PAGE_SIZE } from '$lib/api/pageSizes';
import {
  filteredListState,
  type FilteredListState
} from '$lib/lists/pagination';
import { instant } from '$lib/api/query';
import { dateTimeLocalValue } from '$lib/format';

export type MediaJobForm = {
  route: string;
  jobState: string;
  lifecycle: string;
  apiKeyId: string;
  providerId: string;
  createdAfter: string;
  createdBefore: string;
};

export type MediaJobListState = FilteredListState<
  MediaJobForm,
  Omit<MediaJobFilters, 'cursor'>
>;

export function mediaJobFilters(
  state: MediaJobForm,
  applied?: Omit<MediaJobFilters, 'cursor'>
): Omit<MediaJobFilters, 'cursor'> {
  return {
    limit: MEDIA_JOB_PAGE_SIZE,
    route: state.route.trim() || undefined,
    state: state.jobState || undefined,
    lifecycle: state.lifecycle || undefined,
    api_key_id: state.apiKeyId.trim() || undefined,
    provider_id: state.providerId.trim() || undefined,
    created_after: preservedTime(state.createdAfter, applied?.created_after),
    created_before: preservedTime(state.createdBefore, applied?.created_before)
  };
}

export const mediaJobList = filteredListState({
  emptyForm: (): MediaJobForm => ({
    route: '',
    jobState: '',
    lifecycle: '',
    apiKeyId: '',
    providerId: '',
    createdAfter: '',
    createdBefore: ''
  }),
  toQuery: mediaJobFilters
});

export const mediaJobStates = [
  'queued',
  'running',
  'succeeded',
  'failed',
  'cancelled'
];
export const mediaJobLifecycles = [
  'creating',
  'active',
  'create_ambiguous',
  'create_cleanup_pending',
  'delete_pending',
  'deleted'
];
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export function mediaJobProblem(
  form: MediaJobForm,
  fromUrl = false,
  applied?: Omit<MediaJobFilters, 'cursor'>
): string | null {
  for (const [value, label] of [
    [form.providerId, 'Provider ID'],
    [form.apiKeyId, 'API key ID']
  ]) {
    if (value.trim() && !uuid.test(value.trim()))
      return `${label} must be a UUID: ${value}`;
  }
  if (form.jobState && !mediaJobStates.includes(form.jobState))
    return `Unknown media-job state: ${form.jobState}`;
  if (form.lifecycle && !mediaJobLifecycles.includes(form.lifecycle))
    return `Unknown media-job lifecycle: ${form.lifecycle}`;
  for (const [value, label] of [
    [form.createdAfter, 'Created after'],
    [form.createdBefore, 'Created before']
  ]) {
    if (value.trim() && !mediaJobTimeValid(value, fromUrl))
      return `${label} must be a valid time${fromUrl ? ' with a timezone' : ''}: ${value}`;
  }
  const query = mediaJobFilters(form, applied);
  const after = query.created_after;
  const before = query.created_before;
  if (after && before && timeOrder(after) >= timeOrder(before))
    return 'Created before must be later than created after.';
  return null;
}

export function readMediaJobForm(search: URLSearchParams): MediaJobForm {
  return {
    route: search.get('route')?.trim() ?? '',
    jobState: search.get('state')?.trim() ?? '',
    lifecycle: search.get('lifecycle')?.trim() ?? '',
    apiKeyId: search.get('api_key_id')?.trim() ?? '',
    providerId: search.get('provider_id')?.trim() ?? '',
    createdAfter: search.get('created_after')?.trim() ?? '',
    createdBefore: search.get('created_before')?.trim() ?? ''
  };
}

export function mediaJobState(search: URLSearchParams): MediaJobListState {
  const form = readMediaJobForm(search);
  return {
    ...mediaJobList.empty(),
    ...form,
    applied: mediaJobFilters(form),
    createdAfter: mediaJobTimeValid(form.createdAfter, true)
      ? dateTimeLocalValue(form.createdAfter)
      : form.createdAfter,
    createdBefore: mediaJobTimeValid(form.createdBefore, true)
      ? dateTimeLocalValue(form.createdBefore)
      : form.createdBefore
  };
}

export function mediaJobSearch(
  applied: Omit<MediaJobFilters, 'cursor'>
): string {
  const search = new URLSearchParams();
  for (const name of [
    'route',
    'state',
    'lifecycle',
    'api_key_id',
    'provider_id',
    'created_after',
    'created_before'
  ] as const) {
    const value = applied[name];
    if (value !== undefined && value !== '') search.set(name, value);
  }
  return search.toString();
}

export function mediaJobTimeValid(value: string, fromUrl = false): boolean {
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
  if (!mediaJobTimeValid(value)) return undefined;
  return /(?:Z|[+-]\d{2}:\d{2})$/i.test(value) ? value : instant(value);
}

function timeOrder(value: string): string {
  const fraction = /\.(\d+)/.exec(value)?.[1] ?? '';
  return `${new Date(value).toISOString().slice(0, 19)}.${fraction.padEnd(9, '0')}`;
}
