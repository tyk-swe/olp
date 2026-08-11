import { createContext } from 'svelte';
import type { CursorHistory } from '$lib/api/pagination';

export type ApiKeyListState = CursorHistory;

export const [getApiKeyListState, setApiKeyListState] =
  createContext<ApiKeyListState>();
