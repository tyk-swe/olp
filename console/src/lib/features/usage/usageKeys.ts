import type { RoutingReportFilters } from './usage';
export const usageKeys = {
  routing: (kind: 'savings' | 'shadows', filters: RoutingReportFilters) =>
    ['usage', 'routing', kind, filters] as const,
  report: (applied: string) => ['usage', applied] as const,
  reconciliation: (start: string, end: string) =>
    ['usage', 'reconciliation', start, end] as const
};
