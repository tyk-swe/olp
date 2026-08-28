import {
  emptyCursorHistory,
  listState,
  type CursorHistory
} from '$lib/api/pagination';

export type ApiKeyListState = CursorHistory;

export const apiKeyList = listState<ApiKeyListState>(emptyCursorHistory);

export const { get: getApiKeyListState, set: setApiKeyListState } = apiKeyList;
