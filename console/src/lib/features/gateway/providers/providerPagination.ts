import {
  emptyCursorHistory,
  listState,
  type CursorHistory
} from '$lib/api/pagination';

export const providerList = listState<CursorHistory>(emptyCursorHistory);

export const { get: getProviderPagination, set: setProviderPagination } =
  providerList;
