export const userKeys = {
  page: (cursor?: string) => ['users', 'page', cursor ?? 'first'] as const,
  sessionsRoot: ['users', 'sessions'] as const,
  sessions: (userId: string, cursor?: string) =>
    ['users', 'sessions', userId, cursor ?? 'first'] as const
};
