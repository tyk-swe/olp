import { expect, type Page } from '../playwright';

const point = {
  bucket: '2026-07-12T12:00:00Z',
  request_count: 12,
  input_tokens: '420',
  cached_input_tokens: '96',
  output_tokens: '180',
  media_units: '0',
  estimated_cost: '0.45',
  unpriced_count: 1,
  incomplete_count: 0
};
const coverage = {
  range_complete: false,
  approximate: true,
  excluded_partial_aggregate_boundaries: 1
};
const request_metadata_consumer = {
  state: 'stale',
  pending_events: 4,
  lag_events: 7,
  oldest_pending_at: '2026-07-12T11:59:00Z',
  checked_at: '2026-07-12T12:00:00Z',
  heartbeat_age_seconds: 61
};

export async function mockUsageReport(page: Page) {
  const requests: URL[] = [];
  await page.route('**/api/v1/usage/**', async (route) => {
    const url = new URL(route.request().url());
    requests.push(url);
    const path = url.pathname;
    if (path.endsWith('/summary'))
      await route.fulfill({
        json: {
          request_count: 12,
          input_tokens: '420',
          output_tokens: '180',
          cached_input_tokens: '96',
          media_units: '0',
          estimated_cost: '0.45',
          unpriced_count: 1,
          incomplete_count: 0,
          request_metadata_gap_events: 0,
          uncertain_request_metadata_gap_count: 1,
          coverage,
          request_metadata_consumer,
          complete: false
        }
      });
    else if (path.endsWith('/time-series'))
      await route.fulfill({ json: { items: [point], coverage } });
    else if (path.endsWith('/breakdown'))
      await route.fulfill({
        json: {
          items: [
            {
              dimension: 'support-chat',
              request_count: 12,
              input_tokens: '420',
              cached_input_tokens: '96',
              output_tokens: '180',
              media_units: '0',
              estimated_cost: '0.45',
              unpriced_count: 1,
              incomplete_count: 0
            }
          ],
          coverage
        }
      });
    else
      await route.fulfill({
        json: {
          complete: false,
          request_count: 12,
          priced_count: 11,
          unpriced_count: 1,
          incomplete_count: 0,
          request_metadata_gap_events: 0,
          uncertain_request_metadata_gap_count: 1,
          estimated_cost: '0.45',
          coverage,
          request_metadata_consumer
        }
      });
  });
  return requests;
}

export function expectUsageRequests(
  requests: URL[],
  filters: Record<string, string>
) {
  expect(requests).toHaveLength(4);
  for (const request of requests) {
    for (const [name, value] of Object.entries(filters)) {
      if (name === 'dimension' && !request.pathname.endsWith('breakdown'))
        continue;
      if (name === 'granularity' && !request.pathname.endsWith('time-series'))
        continue;
      expect(request.searchParams.get(name)).toBe(value);
    }
  }
}

export async function mockUsageKey(page: Page, keyId: string) {
  await page.route(`**/api/v1/api-keys/${keyId}`, async (route) => {
    await route.fulfill({
      json: {
        id: keyId,
        lookup_id: 'olp_live_abcd',
        name: 'production SDK',
        scopes: ['inference'],
        allowed_routes: [],
        requests_per_minute: 120,
        tokens_per_minute: 24_000,
        max_concurrency: 8,
        budget: {
          daily: {
            limit: '10.00',
            accrued: '2.50',
            window_ends_at: '2026-07-13T00:00:00Z'
          },
          monthly: {
            limit: '100.00',
            accrued: '18.75',
            window_ends_at: '2026-08-01T00:00:00Z'
          },
          unpriced_attempts: 3
        },
        expires_at: null,
        revoked_at: null,
        rotated_at: null,
        etag: '01980000-0000-7000-8000-000000000105',
        created_by: '01980000-0000-7000-8000-000000000106',
        created_by_email: 'owner@example.com',
        created_at: '2026-07-01T12:00:00Z'
      }
    });
  });
}
