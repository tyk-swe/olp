export const discountKeys = {
  all: ['pricing-discounts'] as const,
  page: (cursor?: string) => ['pricing-discounts', cursor ?? ''] as const
};
export const rateCardKeys = {
  all: ['rate-cards'] as const,
  page: (cursor?: string) => ['rate-cards', cursor ?? ''] as const,
  revisions: (cardId: string, cursor?: string) =>
    ['rate-card-revisions', cardId, cursor ?? ''] as const
};
