export const managementTokenKeys = {
  page: (cursor?: string) =>
    ['management-tokens', 'page', cursor ?? 'first'] as const
};
