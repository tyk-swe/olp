import { createContext } from 'svelte';
import type { RequestFilters } from '$lib/api/requests';
import { REQUEST_PAGE_SIZE } from '$lib/api/pageSizes';
import { emptyCursorHistory, type CursorHistory } from '$lib/api/pagination';
import { instant } from '$lib/api/query';

export type RequestListState = CursorHistory & {
  route: string;
  providerId: string;
  model: string;
  apiKeyId: string;
  operation: string;
  statusCode: string;
  errorClass: string;
  startedAfter: string;
  startedBefore: string;
  applied: Omit<RequestFilters, 'cursor'>;
};

export const [getRequestListState, setRequestListState] =
  createContext<RequestListState>();

export function emptyRequestListState(): RequestListState {
  return {
    ...emptyCursorHistory(),
    route: '',
    providerId: '',
    model: '',
    apiKeyId: '',
    operation: '',
    statusCode: '',
    errorClass: '',
    startedAfter: '',
    startedBefore: '',
    applied: { limit: REQUEST_PAGE_SIZE }
  };
}

export function requestFilters(
  state: RequestListState
): Omit<RequestFilters, 'cursor'> {
  return {
    limit: REQUEST_PAGE_SIZE,
    route: state.route || undefined,
    provider_id: state.providerId || undefined,
    model: state.model || undefined,
    api_key_id: state.apiKeyId || undefined,
    operation: state.operation || undefined,
    status_code: state.statusCode ? Number(state.statusCode) : undefined,
    error_class: state.errorClass || undefined,
    started_after: instant(state.startedAfter),
    started_before: instant(state.startedBefore)
  };
}
