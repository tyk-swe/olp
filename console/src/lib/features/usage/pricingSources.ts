import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { pageResult, result, type CursorPage } from '$lib/api/http';
import { collectCursorPages } from '$lib/api/pagination';

type Schemas = components['schemas'];

export type PricingSource = Schemas['PricingSource'];
export type PricingSourceSnapshot = Schemas['PricingSourceSnapshot'];
export type PricingSourceRefresh = Schemas['PricingSourceRefreshResponse'];
export type PricingSourceDiff = Schemas['PricingSourceDiff'];
export type PublishPricingSourceInput = Schemas['PublishPricingSourceRequest'];

export async function listPricingSources(
  signal?: AbortSignal
): Promise<PricingSource[]> {
  return collectCursorPages((cursor) => listPricingSourcePage(cursor, signal));
}

export async function listPricingSourcePage(
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<PricingSource>> {
  const response = await apiClient.GET('/api/v3/pricing/sources', {
    params: { query: { limit: 50, cursor } },
    signal
  });
  return pageResult(result(response.data, response.error, response.response));
}

export async function createPricingSource(input: {
  name: string;
  url: string;
  enabled?: boolean;
}): Promise<PricingSource> {
  const response = await apiClient.POST('/api/v3/pricing/sources', {
    params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
    body: input
  });
  return result(response.data, response.error, response.response);
}

export async function updatePricingSource(
  source: PricingSource,
  input: { name?: string; url?: string; enabled?: boolean }
): Promise<PricingSource> {
  const response = await apiClient.PATCH(
    '/api/v3/pricing/sources/{pricing_source_id}',
    {
      params: {
        path: { pricing_source_id: source.id },
        header: { 'If-Match': source.etag }
      },
      body: input
    }
  );
  return result(response.data, response.error, response.response);
}

export async function refreshPricingSource(
  source: PricingSource
): Promise<PricingSourceRefresh> {
  const response = await apiClient.POST(
    '/api/v3/pricing/sources/{pricing_source_id}/refresh',
    { params: { path: { pricing_source_id: source.id } } }
  );
  return result(response.data, response.error, response.response);
}

export async function listPricingSourceSnapshots(
  source: PricingSource,
  signal?: AbortSignal
): Promise<PricingSourceSnapshot[]> {
  return collectCursorPages(async (cursor) => {
    const response = await apiClient.GET(
      '/api/v3/pricing/sources/{pricing_source_id}/snapshots',
      {
        params: {
          path: { pricing_source_id: source.id },
          query: { limit: 50, cursor }
        },
        signal
      }
    );
    return pageResult(result(response.data, response.error, response.response));
  });
}

export async function publishPricingSnapshot(
  snapshot: PricingSourceSnapshot,
  input: PublishPricingSourceInput
): Promise<Schemas['PricingRevisionResponse']> {
  const response = await apiClient.POST(
    '/api/v3/pricing/source-snapshots/{pricing_source_snapshot_id}/publish',
    {
      params: {
        path: { pricing_source_snapshot_id: snapshot.id },
        header: { 'Idempotency-Key': crypto.randomUUID() }
      },
      body: input
    }
  );
  return result(response.data, response.error, response.response);
}
