export const auditKeys = {
  page: (applied: unknown, cursor?: string) =>
    ['audit', 'page', applied, cursor ?? 'first'] as const
};
