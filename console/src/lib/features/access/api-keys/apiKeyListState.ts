import type { CursorHistory } from '$lib/api/pagination';

export const apiKeyListStateContext = Symbol('api-key-list-state');

export type ApiKeyListState = CursorHistory;
