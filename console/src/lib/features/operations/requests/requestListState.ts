import type { RequestFilters } from '$lib/api/requests';
import { REQUEST_PAGE_SIZE } from '$lib/api/pageSizes';
import {
  filteredListState,
  type FilteredListState
} from '$lib/lists/pagination';
import { instant } from '$lib/api/query';

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
  state: RequestForm
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
