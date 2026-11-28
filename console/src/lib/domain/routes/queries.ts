import { queryOptions } from '@tanstack/svelte-query';
import { listOperationDialects, listRoutes } from './api.js';
import { routeKeys } from './routeKeys.js';

export const routeQueries = {
  all: () =>
    queryOptions({
      queryKey: routeKeys.all(),
      queryFn: ({ signal }) => listRoutes(signal)
    }),
  dialects: () =>
    queryOptions({
      queryKey: routeKeys.dialects(),
      queryFn: ({ signal }) => listOperationDialects(signal)
    })
};
