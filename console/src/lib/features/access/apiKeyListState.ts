export const apiKeyListStateContext = Symbol('api-key-list-state');

export type ApiKeyListState = {
  cursor: string | undefined;
  history: Array<string | undefined>;
};
