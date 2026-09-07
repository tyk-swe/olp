export const routeKeys = {
  /** Every route or draft listing: the full list and each cursor page. */
  lists: ['routes', 'lists'] as const,
  all: () => ['routes', 'lists', 'all'] as const,
  page: (cursor?: string) =>
    ['routes', 'lists', 'page', cursor ?? 'first'] as const,
  draftPage: (cursor?: string) =>
    ['routes', 'lists', 'drafts', cursor ?? 'first'] as const,
  draft: (id: string) => ['routes', 'draft', id] as const,
  revisions: (routeId: string) => ['routes', 'revisions', routeId] as const
};
