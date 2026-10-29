export const agentKeys = {
  grants: () => ['mcp-grants'] as const,
  servers: () => ['agent-publications', 'servers'] as const,
  runs: (project: string, cursor?: string) =>
    ['agent-runs', project, cursor ?? ''] as const,
  run: (runID: string) => ['agent-run', runID] as const,
  runSpans: (runID: string, cursor?: string) =>
    ['agent-run-spans', runID, cursor ?? ''] as const,
  investigations: (project: string, cursor?: string) =>
    ['investigations', project, cursor ?? ''] as const,
  investigation: (id: string) => ['investigation', id] as const,
  toolCalls: (project: string, key: string, cursor?: string) =>
    ['mcp-tool-calls', project, key, cursor ?? ''] as const
};
