const root = ['users'] as const;
const sessionsRoot = [...root, 'sessions'] as const;

export const userKeys = {
  root,
  page: (cursor?: string) => [...root, 'page', cursor ?? 'first'] as const,
  roster: [...root, 'roster'] as const,
  sessionsRoot,
  sessions: (userId: string, cursor?: string) =>
    [...sessionsRoot, userId, cursor ?? 'first'] as const
};
