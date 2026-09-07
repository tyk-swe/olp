export const pricingKeys = {
  page: (cursor?: string) => ['pricing', 'page', cursor ?? 'first'] as const
};
