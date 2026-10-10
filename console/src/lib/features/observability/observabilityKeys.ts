export const observabilityKeys = {
  root: ['observability'] as const,
  sinks: () => ['observability', 'sinks'] as const,
  capture: () => ['observability', 'capture'] as const,
  capturePolicies: () => ['observability', 'capture-policies'] as const,
  captureSinks: () => ['observability', 'capture-sinks'] as const
};
