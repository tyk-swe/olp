const root = ['providers'] as const;
const summaries = [...root, 'summary'] as const;
const modelCatalog = [...root, 'models', 'catalog'] as const;
const page = (cursor?: string) =>
  [...summaries, 'page', cursor ?? 'first'] as const;
const modelInventory = (cursor?: string) =>
  [...modelCatalog, 'inventory', cursor ?? 'first'] as const;
const revisionsOf = (id: string) => [...root, 'revisions', id] as const;
const revision = (id: string, revisionId: string) =>
  [...revisionsOf(id), 'detail', revisionId] as const;
const modelsOf = (id: string) => [...root, 'models', 'of', id] as const;

export const providerKeys = {
  /** Every provider summary: the full list and each cursor page. */
  summaries,
  all: () => [...summaries, 'all'] as const,
  page,
  filteredPage: (cursor: string | undefined, applied: string) =>
    [...page(cursor), applied] as const,
  detail: (id: string) => [...root, 'detail', id] as const,
  kinds: () => [...root, 'kinds'] as const,
  profiles: () => ['provider-profiles'] as const,
  capabilityOptions: (kind: string) =>
    [...root, 'capability-options', kind] as const,
  credentials: (id: string) => [...root, 'credentials', id] as const,
  revisionsOf,
  revisions: (id: string, cursor?: string) =>
    [...revisionsOf(id), 'page', cursor ?? 'first'] as const,
  revision,
  revisionModels: (id: string, revisionId: string, cursor?: string) =>
    [...revision(id, revisionId), 'models', cursor ?? 'first'] as const,
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
  modelsOf,
  models: (id: string, cursor?: string) =>
    [...modelsOf(id), cursor ?? 'first'] as const,
  vendors: () => ['provider-vendors'] as const,
  catalogSuggestions: (id: string, etag: string) =>
    [...root, 'catalog-suggestions', id, etag] as const,
  configurationSchemas: () => ['provider-configuration-schemas'] as const,
  slots: (id: string, etag: string) => ['provider-slots', id, etag] as const,
  bulkModels: (id: string, etag: string) =>
    ['bulk-provider-models', id, etag] as const,
  networkCredentials: (id: string) =>
    [...root, 'network-credentials', id] as const,
  bulkRoutePreview: () => ['provider-bulk-route-preview'] as const,
  billingImport: (id: string) => [...root, 'billing-import', id] as const
};
