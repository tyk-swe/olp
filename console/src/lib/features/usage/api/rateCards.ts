import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { unwrap, unwrapPage } from '$lib/api/http';
import type { CursorPage } from '$lib/api/http';
import { compactQuery } from '$lib/api/query';

export type RateCard = components['schemas']['RateCard'];
export type RateCardOwner = components['schemas']['RateCardOwner'];
export type RateCardRevision = components['schemas']['RateCardRevision'];
export type CardPriceDraft = components['schemas']['RateCardPriceEntry'];

export async function listRateCards(
  cursor?: string
): Promise<CursorPage<RateCard>> {
  const { data, error, response } = await apiClient.GET('/api/v1/rate-cards', {
    params: { query: compactQuery({ cursor, limit: 25 }) }
  });
  return unwrapPage({ data, error, response });
}

export async function createRateCard(
  owner: {
    kind: 'installation' | 'organization' | 'project';
    id?: string | null;
  },
  requestId: string
): Promise<RateCard> {
  const { data, error, response } = await apiClient.POST('/api/v1/rate-cards', {
    params: { header: { 'Idempotency-Key': requestId } },
    body: { owner }
  });
  return unwrap({ data, error, response });
}

export async function createRateCardRevision(
  cardId: string,
  draft: { effective_at?: string; markup: string; prices?: CardPriceDraft[] },
  requestId: string
): Promise<RateCardRevision> {
  const { data, error, response } = await apiClient.POST(
    '/api/v1/rate-cards/{rate_card_id}/revisions',
    {
      params: {
        path: { rate_card_id: cardId },
        header: { 'Idempotency-Key': requestId }
      },
      body: draft
    }
  );
  return unwrap({ data, error, response });
}
