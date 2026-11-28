import { queryOptions } from '@tanstack/svelte-query';
import { listDefinitions } from './api.js';
import { guardrailKeys } from './guardrailKeys.js';

export const guardrailQueries = {
  definitions: () =>
    queryOptions({
      queryKey: guardrailKeys.definitions(),
      queryFn: ({ signal }) => listDefinitions(signal)
    })
};
