const root = ['observability'] as const;
const sinks = () => [...root, 'sinks'] as const;

export const observabilityKeys = {
  root,
  sinks,
  capture: () => [...root, 'capture'] as const,
  capturePolicies: () => [...root, 'capture-policies'] as const,
  captureSinks: () => [...root, 'capture-sinks'] as const,
  redactorOptions: () => ['capture-redactor-options'] as const,
  gaps: (sink: string) => [...sinks(), 'gaps', sink] as const,
  focusDatasets: (sink: string, cursor?: string) =>
    [...sinks(), sink, 'focus-datasets', cursor] as const
};
