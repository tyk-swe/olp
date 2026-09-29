const root = ['projects'] as const;

export const projectKeys = {
  root,
  page: (cursor?: string) => [...root, 'page', cursor ?? ''] as const,
  membersRoot: [...root, 'members'] as const,
  members: (projectId: string, cursor?: string) =>
    [...root, 'members', projectId, cursor ?? ''] as const,
  memberships: [...root, 'memberships'] as const
};
