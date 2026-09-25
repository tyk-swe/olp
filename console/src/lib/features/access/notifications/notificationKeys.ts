export const notificationKeys = {
  root: ['notifications'] as const,
  destinations: () => ['notifications', 'destinations'] as const,
  rules: () => ['notifications', 'rules'] as const,
  deliveries: (cursor?: string) =>
    ['notifications', 'deliveries', cursor ?? 'first'] as const
};
