export const notificationKeys = {
  root: ['notifications'] as const,
  destinations: () => ['notifications', 'destinations'] as const,
  rules: () => ['notifications', 'rules'] as const,
  deliveries: () => ['notifications', 'deliveries'] as const
};
