import type { RoutingReportFilters, UsageFilters } from './usage';
export const usageKeys = {
  routing: (kind: 'savings' | 'shadows', filters: RoutingReportFilters) =>
    ['usage', 'routing', kind, filters] as const,
  report: (applied: string) => ['usage', applied] as const,
  comparison: (filters: UsageFilters | null) =>
    ['usage', 'comparison', filters] as const,
  unpriced: (filters: UsageFilters) => ['usage', 'unpriced', filters] as const,
  reconciliation: (start: string, end: string) =>
    ['usage', 'reconciliation', start, end] as const
};
