import { projectKeys } from '../projects/projectKeys';

const root = ['scim'] as const;

export const scimKeys = {
  root,
  groups: () => [...root, 'groups'] as const,
  projectOptions: () => [...projectKeys.root, 'scim-options'] as const
};
