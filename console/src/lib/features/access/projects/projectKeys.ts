const root = ['projects'] as const;

export const projectKeys = {
  root,
  page: (cursor?: string) => [...root, 'page', cursor ?? ''] as const,
  all: [...root, 'all'] as const,
  membersRoot: [...root, 'members'] as const,
  members: (projectId: string, cursor?: string) =>
    [...root, 'members', projectId, cursor ?? ''] as const,
  allMembers: (projectId: string) =>
    [...root, 'members', projectId, 'all'] as const,
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
