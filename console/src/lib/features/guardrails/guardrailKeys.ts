const root = ['guardrails'] as const;

export const guardrailKeys = {
  root,
  definitions: () => [...root, 'definitions'] as const,
  policies: () => [...root, 'policies'] as const,
  attachments: () => [...root, 'attachments'] as const,
  projects: () => [...root, 'projects'] as const
};
