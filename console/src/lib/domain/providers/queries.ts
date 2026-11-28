import { queryOptions } from '@tanstack/svelte-query';
import {
  listProviderCredentials,
  listProviderKinds,
  listProviderVendors,
  listProviders
} from './api.js';
import { providerKeys } from './providerKeys.js';

export const providerQueries = {
  all: () =>
    queryOptions({
      queryKey: providerKeys.all(),
      queryFn: ({ signal }) => listProviders(signal)
    }),
  kinds: () =>
    queryOptions({
      queryKey: providerKeys.kinds(),
      queryFn: ({ signal }) => listProviderKinds(signal)
    }),
  vendors: () =>
    queryOptions({
      queryKey: providerKeys.vendors(),
      queryFn: ({ signal }) => listProviderVendors(signal)
    }),
  credentials: (providerId: string) =>
    queryOptions({
      queryKey: providerKeys.credentials(providerId),
      queryFn: ({ signal }) => listProviderCredentials(providerId, signal)
    })
};
