import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { unwrap, unwrapPage } from '$lib/api/http';
import type { CursorPage } from '$lib/api/http';
import { compactQuery } from '$lib/api/query';

export type PricingDiscount = components['schemas']['Discount'];
export type DiscountDraft = components['schemas']['DiscountRequest'];

export async function listDiscounts(
  cursor?: string
): Promise<CursorPage<PricingDiscount>> {
  const { data, error, response } = await apiClient.GET(
    '/api/v1/pricing/discounts',
    { params: { query: compactQuery({ cursor, limit: 25 }) } }
  );
  return unwrapPage({ data, error, response });
}

export async function createDiscount(
  draft: DiscountDraft,
  requestId: string
): Promise<PricingDiscount> {
  const { data, error, response } = await apiClient.POST(
    '/api/v1/pricing/discounts',
    {
      params: { header: { 'Idempotency-Key': requestId } },
      body: draft
    }
  );
  return unwrap({ data, error, response });
}
