import {
  emptyCursorHistory,
  listState,
  type CursorHistory
} from '$lib/lists/pagination';

export type ApiKeyListState = CursorHistory;

export const apiKeyList = listState<ApiKeyListState>(emptyCursorHistory);
