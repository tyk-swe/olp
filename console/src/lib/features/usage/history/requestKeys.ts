export const requestKeys = {
  page: (applied: unknown, cursor?: string) =>
    ['requests', 'page', applied, cursor ?? 'first'] as const,
  detail: (id: string) => ['requests', 'detail', id] as const,
  overview: () => ['requests', 'overview'] as const
};
