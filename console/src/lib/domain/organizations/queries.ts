import { queryOptions } from '@tanstack/svelte-query';
import { listOrganizations } from './api.js';
import { organizationKeys } from './organizationKeys.js';

export const organizationQueries = {
  all: () =>
    queryOptions({
      queryKey: organizationKeys.all,
      queryFn: ({ signal }) => listOrganizations(signal)
    })
};
