import type { Kind } from './api';

const publications = ['agent-publications'] as const;
const publication = <T extends Kind | 'projects' | 'servers'>(kind: T) =>
  [...publications, kind] as const;

export const agentKeys = {
  server: (id: string) => [...publication('servers'), id] as const,
  call: (id: string) => ['mcp-tool-call', id] as const,
  grants: () => ['mcp-grants'] as const,
  publications,
  publication,
  projects: () => publication('projects'),
  servers: () => publication('servers'),
  agents: () => publication('agents'),
  toolsets: () => publication('toolsets'),
  delegationOptions: (keyID: string) =>
    ['agent-delegation-options', keyID] as const,
  runs: (project: string, cursor?: string) =>
    ['agent-runs', project, cursor ?? ''] as const,
  run: (runID: string) => ['agent-run', runID] as const,
  runSpans: (runID: string, cursor?: string, spanID?: string) =>
    ['agent-run-spans', runID, cursor ?? '', spanID ?? ''] as const,
  investigations: (project: string, cursor?: string) =>
    ['investigations', project, cursor ?? ''] as const,
  investigation: (id: string) => ['investigation', id] as const,
  tasks: (project: string, cursor?: string) =>
    ['a2a-tasks', project, cursor ?? ''] as const,
  task: (taskID: string) => ['a2a-task', taskID] as const,
  delegations: (project: string, cursor?: string, delegationID?: string) =>
    ['agent-delegations', project, cursor ?? '', delegationID ?? ''] as const,
  toolCalls: (project: string, key: string, cursor?: string) =>
    ['mcp-tool-calls', project, key, cursor ?? ''] as const
};
