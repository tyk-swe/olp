import {
  emptyCursorHistory,
  listState,
  type CursorHistory
} from '#lib/listing/pagination.js';

export type ApiKeyListState = CursorHistory & { createdBy?: string };

export const apiKeyList = listState<ApiKeyListState>(emptyCursorHistory);
