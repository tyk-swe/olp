import type { RequestQuery } from '$lib/features/usage/history/requestListState';

export const requestKeys = {
  page: (applied: RequestQuery, cursor?: string) =>
    ['requests', 'page', applied, cursor ?? 'first'] as const,
  detail: (id: string) => ['requests', 'detail', id] as const,
  overview: () => ['requests', 'overview'] as const
};
