import {
  emptyCursorHistory,
  listState,
  type CursorHistory
} from '$lib/lists/pagination';

export type ApiKeyListState = CursorHistory & { createdBy?: string };

export const apiKeyList = listState<ApiKeyListState>(emptyCursorHistory);
