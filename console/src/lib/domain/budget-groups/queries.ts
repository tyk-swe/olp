import { queryOptions } from '@tanstack/svelte-query';
import { listBudgetGroups } from './api.js';
import { budgetGroupKeys } from './budgetGroupKeys.js';

export const budgetGroupQueries = {
  all: () =>
    queryOptions({
      queryKey: budgetGroupKeys.list(),
      queryFn: ({ signal }) => listBudgetGroups(signal)
    })
};
