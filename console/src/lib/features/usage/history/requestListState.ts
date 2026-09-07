import {
  operationKinds,
  type RequestFilters
} from '$lib/features/usage/history/api';
import { REQUEST_PAGE_SIZE } from '$lib/api/pageSizes';
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

export type RequestQuery = Omit<RequestFilters, 'cursor'>;
export type RequestListState = FilteredListState<RequestForm, RequestQuery>;

const params = {
  route: 'route',
  providerId: 'provider_id',
  model: 'model',
  apiKeyId: 'api_key_id',
  operation: 'operation',
  statusCode: 'status_code',
  errorClass: 'error_class',
  startedAfter: 'started_after',
  startedBefore: 'started_before'
} as const;

export function requestFilters(
  state: RequestForm,
  applied?: RequestQuery
): RequestQuery {
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

export function requestProblem(
  form: RequestForm,
  fromUrl = false,
  applied?: RequestQuery
): string | null {
  const ids = uuidProblem([
    [form.providerId, 'Provider ID'],
    [form.apiKeyId, 'API key ID']
  ]);
  if (ids) return ids;
  const operation = form.operation.trim();
  if (operation && !(operationKinds as readonly string[]).includes(operation))
    return `Unknown operation: ${form.operation}`;
  if (
    form.statusCode &&
    (!/^\d+$/.test(form.statusCode) || Number(form.statusCode) > 65535)
  )
    return `Status code must be an integer from 0 to 65535: ${form.statusCode}`;
  const times = timeProblem(
    [
      [form.startedAfter, 'Started after'],
      [form.startedBefore, 'Started before']
    ],
    fromUrl
  );
  if (times) return times;
  const query = requestFilters(form, applied);
  return rangeProblem(
    query.started_after,
    query.started_before,
    'Started before must be later than started after.'
  );
}

export function readRequestForm(search: URLSearchParams): RequestForm {
  return readForm<RequestForm>(search, params);
}

export function requestState(search: URLSearchParams): RequestListState {
  const form = readRequestForm(search);
  return {
    ...requestList.empty(),
    ...form,
    applied: requestFilters(form),
    startedAfter: formTimeValue(form.startedAfter),
    startedBefore: formTimeValue(form.startedBefore)
  };
}

export function requestSearch(applied: RequestQuery): string {
  return querySearch(applied, Object.values(params));
}

export const requestUrl: ListUrlSpec<RequestListState> = {
  path: '/requests',
  state: requestState,
  canonical: (search) => {
    const form = readRequestForm(search);
    return requestProblem(form, true)
      ? null
      : requestSearch(requestFilters(form));
  }
};
