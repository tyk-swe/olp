export const apiKeyQueries = {
  root: ['api-keys'] as const,
  hasNonrevoked: () => ['api-keys', 'has-nonrevoked'] as const,
  list: () => ['api-keys', 'list'] as const,
  page: (cursor?: string, createdBy?: string) =>
    ['api-keys', 'page', createdBy ?? 'all', cursor ?? 'first'] as const
};
