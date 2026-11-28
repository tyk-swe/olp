import { queryOptions } from '@tanstack/svelte-query';
import { listUsers } from './api.js';
import { userKeys } from './userKeys.js';

export const userQueries = {
  all: () =>
    queryOptions({
      queryKey: userKeys.roster,
      queryFn: ({ signal }) => listUsers(signal)
    })
};
