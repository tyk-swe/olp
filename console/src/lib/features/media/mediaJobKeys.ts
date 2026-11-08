import type { MediaJobQuery } from '#lib/features/media/mediaJobListState.js';

const root = ['media-jobs'] as const;

export const mediaJobKeys = {
  all: root,
  page: (applied: MediaJobQuery, cursor?: string) =>
    [...root, 'page', applied, cursor ?? 'first'] as const,
  detail: (id: string) => [...root, 'detail', id] as const
};
