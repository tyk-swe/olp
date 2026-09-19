export const overviewKeys = {
  /** Every overview aggregate; mutations in any covered domain invalidate it. */
  root: ['overview'] as const,
  summary: () => ['overview', 'summary'] as const
};
