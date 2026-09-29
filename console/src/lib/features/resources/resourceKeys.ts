import type { ProviderResourceFilters } from './api';

export const resourceKeys = {
  page: (applied: Omit<ProviderResourceFilters, 'cursor'>, cursor?: string) =>
    ['provider-resources', applied, cursor] as const
};
