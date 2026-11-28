const root = ['scim'] as const;

export const scimKeys = {
  root,
  groups: () => [...root, 'groups'] as const
};
