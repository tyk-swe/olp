import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { unwrap } from '$lib/api/http';

export type PricingQuoteRequest = components['schemas']['PricingQuoteRequest'];
export type PricingQuote = components['schemas']['PricingQuoteResponse'];

/** Prices one attempt's usage exactly as accounting would record it. */
export async function quoteUsage(
  body: PricingQuoteRequest
): Promise<PricingQuote> {
  const { data, error, response } = await apiClient.POST(
    '/api/v1/pricing/quote',
    { body }
  );
  return unwrap({ data, error, response });
}
