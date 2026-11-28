import { projectKeys } from '#lib/domain/projects/projectKeys.js';

const root = [...projectKeys.root, 'organizations'] as const;

export const organizationKeys = {
  root,
  all: [...root, 'all'] as const,
  members: (organizationId: string) =>
    [...root, organizationId, 'members'] as const,
  projects: (organizationId: string) =>
    [...root, organizationId, 'projects'] as const,
  projectMembers: (organizationId: string, projectId: string) =>
    [...root, organizationId, projectId, 'members'] as const
};
