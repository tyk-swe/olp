export const agentKeys = {
  grants: () => ['mcp-grants'] as const,
  servers: () => ['agent-publications', 'servers'] as const,
  toolCalls: (project: string, key: string, cursor?: string) =>
    ['mcp-tool-calls', project, key, cursor ?? ''] as const
};
