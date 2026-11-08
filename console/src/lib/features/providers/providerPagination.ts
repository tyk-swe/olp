import {
  emptyCursorHistory,
  listState,
  type CursorHistory
} from '#lib/listing/pagination.js';

export type ProviderListState = CursorHistory & {
  /** Draft text in the search box. */
  search: string;
  /** The search the current page request reflects. */
  applied: string;
};

export const providerList = listState<ProviderListState>(() => ({
  ...emptyCursorHistory(),
  search: '',
  applied: ''
}));
