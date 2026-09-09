import type { MediaJobQuery } from '$lib/features/media/mediaJobListState';

export const mediaJobKeys = {
  page: (applied: MediaJobQuery, cursor?: string) =>
    ['media-jobs', 'page', applied, cursor ?? 'first'] as const,
  detail: (id: string) => ['media-jobs', 'detail', id] as const
};
