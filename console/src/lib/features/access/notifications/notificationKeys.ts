const root = ['notifications'] as const;

export const notificationKeys = {
  root,
  destinations: () => [...root, 'destinations'] as const,
  rules: () => [...root, 'rules'] as const,
  deliveries: (cursor?: string) => [...root, 'deliveries', cursor] as const
};
