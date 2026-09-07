import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { pageResult, result } from '$lib/api/http';
import type { CursorPage } from '$lib/api/http';
import { compactQuery } from '$lib/api/query';

export type PricingRevision = components['schemas']['PricingRevisionResponse'];
export type PriceDraft = components['schemas']['PriceRequest'];

export async function listPricing(
  cursor?: string
): Promise<CursorPage<PricingRevision>> {
  const { data, error, response } = await apiClient.GET(
    '/api/v3/pricing/revisions',
    { params: { query: compactQuery({ cursor, limit: 25 }) } }
  );
  return pageResult(result(data, error, response));
}

export async function createPricingRevision(
  effectiveAt: string,
  prices: PriceDraft[]
): Promise<PricingRevision> {
  const { data, error, response } = await apiClient.POST(
    '/api/v3/pricing/revisions',
    {
      params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
      body: { effective_at: effectiveAt, prices }
    }
  );
  return result(data, error, response);
}
