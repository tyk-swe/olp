const root = ['overview'] as const;

export const overviewKeys = {
  /** Every overview aggregate; mutations in any covered domain invalidate it. */
  root,
  summary: () => [...root, 'summary'] as const
};
