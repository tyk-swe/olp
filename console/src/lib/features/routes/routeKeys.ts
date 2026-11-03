const root = ['routes'] as const;
const lists = [...root, 'lists'] as const;

export const routeKeys = {
  /** Every route or draft listing: the full list and each cursor page. */
  lists,
  all: () => [...lists, 'all'] as const,
  page: (cursor?: string) => [...lists, 'page', cursor ?? 'first'] as const,
  draftPage: (cursor?: string) =>
    [...lists, 'drafts', cursor ?? 'first'] as const,
  draft: (id: string) => [...root, 'draft', id] as const,
  revisions: (routeId: string) => [...root, 'revisions', routeId] as const,
  policy: <Scope extends string>(scope: Scope, id: string, etag: string) =>
    ['routing-policy', scope, id, etag] as const
};
