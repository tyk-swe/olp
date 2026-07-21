export const providerPaginationContext = Symbol('provider-pagination');

export type ProviderPagination = {
  readonly cursor: string | undefined;
  readonly history: Array<string | undefined>;
  setCursor: (cursor: string | undefined) => void;
  setHistory: (history: Array<string | undefined>) => void;
};
