import type { ProviderResourceFilters } from './api';

export const resourceKeys = {
  page: (applied: Omit<ProviderResourceFilters, 'cursor'>, cursor?: string) =>
    ['provider-resources', applied, cursor] as const,
  contracts: () => ['provider-resource-contracts'] as const,
  retrievalBindings: () => ['retrieval-bindings'] as const
};
