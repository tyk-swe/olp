import type { RequestFilters } from '$lib/api/requests';
import { REQUEST_PAGE_SIZE } from '$lib/api/pageSizes';
import {
  filteredListState,
  type FilteredListState
} from '$lib/lists/pagination';
import { instant } from '$lib/api/query';
import { dateTimeLocalValue } from '$lib/format';

export type RequestForm = {
  route: string;
  providerId: string;
  model: string;
  apiKeyId: string;
  operation: string;
  statusCode: string;
  errorClass: string;
  startedAfter: string;
  startedBefore: string;
};

export type RequestListState = FilteredListState<
  RequestForm,
  Omit<RequestFilters, 'cursor'>
>;

export function requestFilters(
  state: RequestForm,
  applied?: Omit<RequestFilters, 'cursor'>
): Omit<RequestFilters, 'cursor'> {
  return {
    limit: REQUEST_PAGE_SIZE,
    route: state.route.trim() || undefined,
    provider_id: state.providerId.trim() || undefined,
    model: state.model.trim() || undefined,
    api_key_id: state.apiKeyId.trim() || undefined,
    operation: state.operation.trim() || undefined,
    status_code: state.statusCode ? Number(state.statusCode) : undefined,
    error_class: state.errorClass.trim() || undefined,
    started_after: preservedTime(state.startedAfter, applied?.started_after),
    started_before: preservedTime(state.startedBefore, applied?.started_before)
  };
}

export const requestList = filteredListState({
  emptyForm: (): RequestForm => ({
    route: '',
    providerId: '',
    model: '',
    apiKeyId: '',
    operation: '',
    statusCode: '',
    errorClass: '',
    startedAfter: '',
    startedBefore: ''
  }),
  toQuery: requestFilters
});

const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const operations = [
  'generation',
  'embeddings',
  'token_count',
  'image_generation',
  'image_edit',
  'image_variation',
  'speech',
  'transcription',
  'video_create',
  'video_list',
  'video_get',
  'video_content',
  'video_delete',
  'moderation',
  'model_list',
  'model_get'
];

export function requestProblem(
  form: RequestForm,
  fromUrl = false,
  applied?: Omit<RequestFilters, 'cursor'>
): string | null {
  for (const [value, label] of [
    [form.providerId, 'Provider ID'],
    [form.apiKeyId, 'API key ID']
  ]) {
    if (value.trim() && !uuid.test(value.trim()))
      return `${label} must be a UUID: ${value}`;
  }
  if (form.operation.trim() && !operations.includes(form.operation.trim()))
    return `Unknown operation: ${form.operation}`;
  if (
    form.statusCode &&
    (!/^\d+$/.test(form.statusCode) || Number(form.statusCode) > 65535)
  )
    return `Status code must be an integer from 0 to 65535: ${form.statusCode}`;
  for (const [value, label] of [
    [form.startedAfter, 'Started after'],
    [form.startedBefore, 'Started before']
  ]) {
    if (value.trim() && !requestTimeValid(value, fromUrl))
      return `${label} must be a valid time${fromUrl ? ' with a timezone' : ''}: ${value}`;
  }
  const query = requestFilters(form, applied);
  const after = query.started_after;
  const before = query.started_before;
  if (after && before && timeOrder(after) >= timeOrder(before))
    return 'Started before must be later than started after.';
  return null;
}

export function readRequestForm(search: URLSearchParams): RequestForm {
  return {
    route: search.get('route')?.trim() ?? '',
    providerId: search.get('provider_id')?.trim() ?? '',
    model: search.get('model')?.trim() ?? '',
    apiKeyId: search.get('api_key_id')?.trim() ?? '',
    operation: search.get('operation')?.trim() ?? '',
    statusCode: search.get('status_code')?.trim() ?? '',
    errorClass: search.get('error_class')?.trim() ?? '',
    startedAfter: search.get('started_after')?.trim() ?? '',
    startedBefore: search.get('started_before')?.trim() ?? ''
  };
}

export function requestState(search: URLSearchParams): RequestListState {
  const form = readRequestForm(search);
  return {
    ...requestList.empty(),
    ...form,
    applied: requestFilters(form),
    startedAfter: requestTimeValid(form.startedAfter, true)
      ? dateTimeLocalValue(form.startedAfter)
      : form.startedAfter,
    startedBefore: requestTimeValid(form.startedBefore, true)
      ? dateTimeLocalValue(form.startedBefore)
      : form.startedBefore
  };
}

export function requestSearch(applied: Omit<RequestFilters, 'cursor'>): string {
  const search = new URLSearchParams();
  for (const name of [
    'route',
    'provider_id',
    'model',
    'api_key_id',
    'operation',
    'status_code',
    'error_class',
    'started_after',
    'started_before'
  ] as const) {
    const value = applied[name];
    if (value !== undefined && value !== '') search.set(name, String(value));
  }
  return search.toString();
}

export function requestTimeValid(value: string, fromUrl = false): boolean {
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
  if (!requestTimeValid(value)) return undefined;
  return /(?:Z|[+-]\d{2}:\d{2})$/i.test(value) ? value : instant(value);
}

function timeOrder(value: string): string {
  const fraction = /\.(\d+)/.exec(value)?.[1] ?? '';
  return `${new Date(value).toISOString().slice(0, 19)}.${fraction.padEnd(9, '0')}`;
}
