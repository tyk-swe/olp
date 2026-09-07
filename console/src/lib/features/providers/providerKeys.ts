export const providerKeys = {
  /** Every provider summary: the full list and each cursor page. */
  summaries: ['providers', 'summary'] as const,
  all: () => ['providers', 'summary', 'all'] as const,
  page: (cursor?: string) =>
    ['providers', 'summary', 'page', cursor ?? 'first'] as const,
  detail: (id: string) => ['providers', 'detail', id] as const,
  kinds: () => ['providers', 'kinds'] as const,
  capabilityOptions: (kind: string) =>
    ['providers', 'capability-options', kind] as const,
  credentials: (id: string) => ['providers', 'credentials', id] as const,
  revisionsOf: (id: string) => ['providers', 'revisions', id] as const,
  revisions: (id: string, cursor?: string) =>
    ['providers', 'revisions', id, 'page', cursor ?? 'first'] as const,
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
      cursor ?? 'first'
    ] as const,
  /** Model views that change whenever any provider's models change. */
  modelCatalog: ['providers', 'models', 'catalog'] as const,
  modelInventory: (cursor?: string) =>
    ['providers', 'models', 'catalog', 'inventory', cursor ?? 'first'] as const,
  enabledModels: () => ['providers', 'models', 'catalog', 'enabled'] as const,
  modelsOf: (id: string) => ['providers', 'models', 'of', id] as const,
  models: (id: string, cursor?: string) =>
    ['providers', 'models', 'of', id, cursor ?? 'first'] as const
};
