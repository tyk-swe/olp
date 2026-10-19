const root = ['model-catalog'] as const;

export const catalogKeys = {
  root,
  publicCatalog: (projectId?: string) => {
    if (!projectId) return root;
    return [...root, 'public', projectId] as const;
  },
  exposureRoutes: () => [...root, 'exposure-routes'] as const,
  publication: (id: string) => [...root, 'publication', id] as const,
  exposure: (id: string) => [...root, 'exposure', id] as const
};
