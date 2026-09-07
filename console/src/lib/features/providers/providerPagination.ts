import {
  emptyCursorHistory,
  listState,
  type CursorHistory
} from '$lib/lists/pagination';

export const providerList = listState<CursorHistory>(emptyCursorHistory);
