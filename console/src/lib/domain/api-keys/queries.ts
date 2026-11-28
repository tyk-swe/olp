import { queryOptions } from '@tanstack/svelte-query';
import { listApiKeys } from './api.js';
import { apiKeyKeys } from './apiKeyKeys.js';

export const apiKeyQueries = {
  all: () =>
    queryOptions({
      queryKey: apiKeyKeys.list(),
      queryFn: ({ signal }) => listApiKeys(signal)
    })
};
