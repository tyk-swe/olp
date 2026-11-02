const discounts = ['pricing-discounts'] as const;
const rateCards = ['rate-cards'] as const;

export const discountKeys = {
  all: discounts,
  page: (cursor?: string) => [...discounts, cursor ?? ''] as const
};
export const rateCardKeys = {
  all: rateCards,
  page: (cursor?: string) => [...rateCards, cursor ?? ''] as const,
  revisions: (cardId: string, cursor?: string) =>
    ['rate-card-revisions', cardId, cursor ?? ''] as const
};
