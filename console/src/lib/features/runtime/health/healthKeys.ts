export const healthKeys = {
  readiness: () => ['health', 'readiness'] as const,
  providers: (windowMinutes: number) =>
    ['health', 'providers', windowMinutes] as const,
  persistence: () => ['health', 'persistence'] as const,
  generations: (cursor?: string) =>
    ['health', 'generations', cursor ?? 'first'] as const,
  epochs: (cursor?: string) => ['health', 'epochs', cursor ?? 'first'] as const
};
