const root = ['settings'] as const;

export const settingsKeys = {
  all: () => root,
  branding: [...root, 'branding'] as const
};
