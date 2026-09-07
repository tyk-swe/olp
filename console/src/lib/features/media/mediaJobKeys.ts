export const mediaJobKeys = {
  page: (applied: unknown, cursor?: string) =>
    ['media-jobs', 'page', applied, cursor ?? 'first'] as const,
  detail: (id: string) => ['media-jobs', 'detail', id] as const
};
