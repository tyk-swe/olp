/**
 * Every TanStack query key the console uses, built here so list, page, and
 * detail keys of one resource share a prefix. A mutation then invalidates one
 * prefix instead of remembering which flat string keys belong together.
 */
const first = (cursor?: string | null) => cursor ?? 'first';

export const queryKeys = {
  providers: {
    /** Every provider summary: the full list and each cursor page. */
    summaries: ['providers', 'summary'] as const,
    all: () => ['providers', 'summary', 'all'] as const,
    page: (cursor?: string) =>
      ['providers', 'summary', 'page', first(cursor)] as const,
    detail: (id: string) => ['providers', 'detail', id] as const,
    kinds: () => ['providers', 'kinds'] as const,
    capabilityOptions: (kind: string) =>
      ['providers', 'capability-options', kind] as const,
    credentials: (id: string) => ['providers', 'credentials', id] as const,
    revisionsOf: (id: string) => ['providers', 'revisions', id] as const,
    revisions: (id: string, cursor?: string) =>
      ['providers', 'revisions', id, 'page', first(cursor)] as const,
    revision: (id: string, revisionId: string) =>
      ['providers', 'revisions', id, 'detail', revisionId] as const,
    revisionModels: (id: string, revisionId: string, cursor?: string) =>
      [
        'providers',
        'revisions',
        id,
        'detail',
        revisionId,
        'models',
        first(cursor)
      ] as const,
    /** Model views that change whenever any provider's models change. */
    modelCatalog: ['providers', 'models', 'catalog'] as const,
    modelInventory: (cursor?: string) =>
      ['providers', 'models', 'catalog', 'inventory', first(cursor)] as const,
    enabledModels: () => ['providers', 'models', 'catalog', 'enabled'] as const,
    modelsOf: (id: string) => ['providers', 'models', 'of', id] as const,
    models: (id: string, cursor?: string) =>
      ['providers', 'models', 'of', id, first(cursor)] as const
  },
  routes: {
    /** Every route or draft listing: the full list and each cursor page. */
    lists: ['routes', 'lists'] as const,
    all: () => ['routes', 'lists', 'all'] as const,
    page: (cursor?: string) =>
      ['routes', 'lists', 'page', first(cursor)] as const,
    draftPage: (cursor?: string) =>
      ['routes', 'lists', 'drafts', first(cursor)] as const,
    draft: (id: string) => ['routes', 'draft', id] as const,
    revisions: (routeId: string) => ['routes', 'revisions', routeId] as const
  },
  apiKeys: {
    root: ['api-keys'] as const,
    all: () => ['api-keys', 'all'] as const,
    page: (cursor?: string) => ['api-keys', 'page', first(cursor)] as const
  },
  users: {
    page: (cursor?: string) => ['users', 'page', first(cursor)] as const,
    sessionsRoot: ['users', 'sessions'] as const,
    sessions: (userId: string, cursor?: string) =>
      ['users', 'sessions', userId, first(cursor)] as const
  },
  invitations: {
    page: (cursor?: string) => ['invitations', 'page', first(cursor)] as const
  },
  oidc: {
    configuration: () => ['oidc', 'configuration'] as const
  },
  audit: {
    page: (applied: unknown, cursor?: string) =>
      ['audit', 'page', applied, first(cursor)] as const
  },
  requests: {
    page: (applied: unknown, cursor?: string) =>
      ['requests', 'page', applied, first(cursor)] as const,
    detail: (id: string) => ['requests', 'detail', id] as const,
    overview: () => ['requests', 'overview'] as const
  },
  mediaJobs: {
    page: (applied: unknown, cursor?: string) =>
      ['media-jobs', 'page', applied, first(cursor)] as const,
    detail: (id: string) => ['media-jobs', 'detail', id] as const
  },
  health: {
    readiness: () => ['health', 'readiness'] as const,
    providers: (windowMinutes: number) =>
      ['health', 'providers', windowMinutes] as const,
    persistence: () => ['health', 'persistence'] as const,
    generations: (cursor?: string) =>
      ['health', 'generations', first(cursor)] as const,
    epochs: (cursor?: string) => ['health', 'epochs', first(cursor)] as const
  },
  usage: {
    report: (applied: string) => ['usage', applied] as const
  },
  settings: {
    all: () => ['settings'] as const
  },
  pricing: {
    page: (cursor?: string) => ['pricing', 'page', first(cursor)] as const
  },
  profile: {
    current: () => ['profile'] as const,
    sessionsRoot: ['profile', 'sessions'] as const,
    sessions: (cursor?: string) =>
      ['profile', 'sessions', first(cursor)] as const,
    oidcIdentities: () => ['profile', 'oidc-identities'] as const
  }
};
