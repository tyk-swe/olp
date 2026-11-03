const root = ['api-keys'] as const;

export const apiKeyKeys = {
  root,
  list: () => [...root, 'list'] as const,
  page: (cursor?: string, createdBy?: string) =>
    [...root, 'page', createdBy ?? 'all', cursor ?? 'first'] as const
};
