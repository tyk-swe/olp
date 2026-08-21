import type { components } from './schema';
import { apiClient } from './client';
import { result } from './http';
import type { CursorPage } from './pagination';
import { compactQuery } from './query';

export type PricingRevision = components['schemas']['PricingRevisionResponse'];
export type PriceDraft = components['schemas']['PriceRequest'];

export async function listPricing(
  cursor?: string
): Promise<CursorPage<PricingRevision>> {
  const { data, error, response } = await apiClient.GET(
    '/api/v1/pricing/revisions',
    { params: { query: compactQuery({ cursor, limit: 25 }) } }
  );
  const page = result(data, error, response);
  return { items: page.data, nextCursor: page.next_cursor ?? null };
}

export async function createPricingRevision(
  effectiveAt: string,
  prices: PriceDraft[]
): Promise<PricingRevision> {
  const { data, error, response } = await apiClient.POST(
    '/api/v1/pricing/revisions',
    {
      params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
      body: { effective_at: effectiveAt, prices }
    }
  );
  return result(data, error, response);
}
