import { projectKeys } from '../projects/projectKeys';

const root = [...projectKeys.root, 'organizations'] as const;

export const organizationKeys = {
  root,
  all: ['organizations'] as const,
  members: (organizationId: string) =>
    [...root, organizationId, 'members'] as const,
  projects: (organizationId: string) =>
    [...root, organizationId, 'projects'] as const,
  projectMembers: (organizationId: string, projectId: string) =>
    [...root, organizationId, projectId, 'members'] as const
};
