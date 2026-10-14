export const usageKeys = {
  report: (applied: string) => ['usage', applied] as const,
  reconciliation: (start: string, end: string) =>
    ['usage', 'reconciliation', start, end] as const
};
