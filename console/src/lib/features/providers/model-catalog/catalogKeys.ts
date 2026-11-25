const root = ['model-catalog'] as const;

export const catalogKeys = {
  root,
  publicCatalog: (projectId?: string) => {
    if (!projectId) return root;
    return [...root, 'public', projectId] as const;
  },
  exposure: (id: string) => [...root, 'exposure', id] as const
};
