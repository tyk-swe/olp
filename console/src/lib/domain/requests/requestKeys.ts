import type { RequestFilters } from './api.js';

export const requestKeys = {
  page: (applied: Omit<RequestFilters, 'cursor'>, cursor?: string) =>
    ['requests', 'page', applied, cursor ?? 'first'] as const,
  detail: (id: string) => ['requests', 'detail', id] as const,
  overview: () => ['requests', 'overview'] as const
};
