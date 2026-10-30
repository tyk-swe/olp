const sinks = () => ['observability', 'sinks'] as const;

export const observabilityKeys = {
  root: ['observability'] as const,
  sinks,
  capture: () => ['observability', 'capture'] as const,
  capturePolicies: () => ['observability', 'capture-policies'] as const,
  captureSinks: () => ['observability', 'capture-sinks'] as const,
  redactorOptions: () => ['capture-redactor-options'] as const,
  gaps: (sink: string) => [...sinks(), 'gaps', sink] as const,
  focusDatasets: (sink: string, cursor?: string) =>
    ['observability', 'sinks', sink, 'focus-datasets', cursor] as const
};
