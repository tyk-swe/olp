import type { CursorHistory } from '$lib/api/pagination';

export const providerPaginationContext = Symbol('provider-pagination');

export type ProviderPagination = Readonly<CursorHistory> & {
  setCursor: (cursor: string | undefined) => void;
  setHistory: (history: Array<string | undefined>) => void;
};
