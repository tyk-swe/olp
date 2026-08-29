import {
  emptyCursorHistory,
  listState,
  type CursorHistory
} from '$lib/api/pagination';

export const providerList = listState<CursorHistory>(emptyCursorHistory);
