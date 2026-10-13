export const discountKeys = {
  page: (cursor?: string) => ['pricing-discounts', cursor ?? ''] as const
};
export const rateCardKeys = {
  page: (cursor?: string) => ['rate-cards', cursor ?? ''] as const,
  revisions: (cardId: string, cursor?: string) =>
    ['rate-card-revisions', cardId, cursor ?? ''] as const
};
