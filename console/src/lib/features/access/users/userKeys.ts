export const userKeys = {
  root: ['users'] as const,
  page: (cursor?: string) => ['users', 'page', cursor ?? 'first'] as const,
  roster: ['users', 'roster'] as const,
  sessions: (userId: string, cursor?: string) =>
    ['users', 'sessions', userId, cursor ?? 'first'] as const
};
