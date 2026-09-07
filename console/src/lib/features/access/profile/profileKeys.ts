export const profileKeys = {
  current: () => ['profile'] as const,
  sessionsRoot: ['profile', 'sessions'] as const,
  sessions: (cursor?: string) =>
    ['profile', 'sessions', cursor ?? 'first'] as const,
  oidcIdentities: () => ['profile', 'oidc-identities'] as const
};
