import type { CodeFilters } from '$lib/api/code-mode';

const root = ['code-mode'] as const;
export const codeKeys = {
  root,
  collection: (name: string, filters: CodeFilters) =>
    [...root, name, filters] as const,
  inventory: (project: string) => [...root, 'inventory', project] as const,
  revisions: (id: string, cursor?: string) =>
    [...root, 'revisions', id, cursor ?? ''] as const,
  configuration: (id: string, revision: string) =>
    [...root, 'configuration', id, revision] as const
};
