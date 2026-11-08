const root = ['projects'] as const;
const membersRoot = [...root, 'members'] as const;

export const projectKeys = {
  root,
  page: (cursor?: string) => [...root, 'page', cursor ?? ''] as const,
  all: [...root, 'all'] as const,
  detail: (projectId: string) => [...root, 'detail', projectId] as const,
  membersRoot,
  members: (projectId: string, cursor?: string) =>
    [...membersRoot, projectId, cursor ?? ''] as const,
  allMembers: (projectId: string) =>
    [...membersRoot, projectId, 'all'] as const,
  memberships: [...root, 'memberships'] as const,
  attributionPolicy: (projectId: string) =>
    [...root, 'attribution-policy', projectId] as const,
  attributionBudgets: (projectId: string) =>
    [...root, 'attribution-budgets', projectId] as const,
  endUserPolicy: (projectId: string) =>
    [...root, 'end-user-policy', projectId] as const,
  routeGroups: (projectId: string) =>
    [...root, 'route-groups', projectId] as const,
  limitTemplates: (projectId: string) =>
    [...root, 'limit-templates', projectId] as const
};
