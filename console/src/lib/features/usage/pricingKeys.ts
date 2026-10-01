export const pricingKeys = {
  root: ['pricing'] as const,
  pages: () => ['pricing', 'page'] as const,
  page: (cursor?: string) => ['pricing', 'page', cursor ?? 'first'] as const,
  sources: () => ['pricing', 'sources'] as const
};
