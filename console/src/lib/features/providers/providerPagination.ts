import {
  emptyCursorHistory,
  listState,
  type CursorHistory
} from '$lib/lists/pagination';

export type ProviderListState = CursorHistory & { search: string };

export const providerList = listState<ProviderListState>(() => ({
  ...emptyCursorHistory(),
  search: ''
}));
