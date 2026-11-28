import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/session/client.js';
import { unwrap, unwrapPage, createHeaders } from '#lib/management/http.js';
import type { CursorPage } from '#lib/management/http.js';
import { compactQuery } from '#lib/management/query.js';

export type RateCard = components['schemas']['RateCard'];
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
    kind: components['schemas']['RateCardOwner']['kind'];
    id?: string | null;
  },
  requestId: string
): Promise<RateCard> {
  const { data, error, response } = await apiClient.POST('/api/v1/rate-cards', {
    params: { header: createHeaders(requestId) },
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
        header: createHeaders(requestId)
      },
      body: draft
    }
  );
  return unwrap({ data, error, response });
}
