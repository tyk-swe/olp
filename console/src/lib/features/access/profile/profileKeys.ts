const root = ['profile'] as const;
const sessionsRoot = [...root, 'sessions'] as const;

export const profileKeys = {
  current: () => root,
  sessionsRoot,
  sessions: (cursor?: string) => [...sessionsRoot, cursor ?? 'first'] as const,
  oidcIdentities: () => [...root, 'oidc-identities'] as const,
  saml: () => [...root, 'saml'] as const,
  mfa: () => [...root, 'mfa'] as const
};
