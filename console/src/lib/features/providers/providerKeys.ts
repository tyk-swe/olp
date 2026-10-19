const summaries = ['providers', 'summary'] as const;
const modelCatalog = ['providers', 'models', 'catalog'] as const;
const page = (cursor?: string) =>
  [...summaries, 'page', cursor ?? 'first'] as const;
const modelInventory = (cursor?: string) =>
  [...modelCatalog, 'inventory', cursor ?? 'first'] as const;

export const providerKeys = {
  /** Every provider summary: the full list and each cursor page. */
  summaries,
  all: () => [...summaries, 'all'] as const,
  page,
  filteredPage: (cursor: string | undefined, applied: string) =>
    [...page(cursor), applied] as const,
  detail: (id: string) => ['providers', 'detail', id] as const,
  kinds: () => ['providers', 'kinds'] as const,
  profiles: () => ['provider-profiles'] as const,
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
  modelCatalog,
  modelInventory,
  filteredModelInventory: (
    cursor: string | undefined,
    search: string,
    surface: string,
    eligibility: string
  ) => [...modelInventory(cursor), search, surface, eligibility] as const,
  enabledModels: () => [...modelCatalog, 'enabled'] as const,
  modelsOf: (id: string) => ['providers', 'models', 'of', id] as const,
  models: (id: string, cursor?: string) =>
    ['providers', 'models', 'of', id, cursor ?? 'first'] as const,
  vendors: () => ['provider-vendors'] as const,
  catalogSuggestions: (id: string, etag: string) =>
    ['providers', 'catalog-suggestions', id, etag] as const,
  configurationSchemas: () => ['provider-configuration-schemas'] as const,
  slots: (id: string, etag: string) => ['provider-slots', id, etag] as const,
  bulkModels: (id: string, etag: string) =>
    ['bulk-provider-models', id, etag] as const,
  networkCredentials: (id: string) =>
    ['providers', 'network-credentials', id] as const,
  bulkRoutePreview: () => ['provider-bulk-route-preview'] as const,
  billingImport: (id: string) => ['providers', 'billing-import', id] as const
};
