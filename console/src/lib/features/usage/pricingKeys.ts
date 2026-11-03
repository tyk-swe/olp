const root = ['pricing'] as const;
const pages = () => [...root, 'page'] as const;

export const pricingKeys = {
  root,
  pages,
  page: (cursor?: string) => [...pages(), cursor ?? 'first'] as const,
  sources: () => [...root, 'sources'] as const,
  referenceCatalog: () => [...root, 'reference-catalog'] as const
};
