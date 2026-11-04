import type { components } from '$lib/management/schema';
import { apiClient } from '$lib/management/client';
import { unwrap, unwrapPage } from '$lib/management/http';
import type { CursorPage } from '$lib/management/http';
import { compactQuery } from '$lib/management/query';

export type PricingRevision = components['schemas']['PricingRevisionResponse'];
export type PriceDraft = components['schemas']['PriceRequest'];

export async function listPricing(
  cursor?: string
): Promise<CursorPage<PricingRevision>> {
  const { data, error, response } = await apiClient.GET(
    '/api/v1/pricing/revisions',
    { params: { query: compactQuery({ cursor, limit: 25 }) } }
  );
  return unwrapPage({ data, error, response });
}

export async function createPricingRevision(
  effectiveAt: string,
  prices: PriceDraft[],
  requestId: string
): Promise<PricingRevision> {
  const { data, error, response } = await apiClient.POST(
    '/api/v1/pricing/revisions',
    {
      params: { header: { 'Idempotency-Key': requestId } },
      body: { effective_at: effectiveAt, prices }
    }
  );
  return unwrap({ data, error, response });
}
