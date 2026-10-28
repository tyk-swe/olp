export const agentKeys = {
  toolCalls: (project: string, key: string, cursor?: string) =>
    ['mcp-tool-calls', project, key, cursor ?? ''] as const
};
