import { afterEach, describe, expect, it, vi } from 'vitest';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import { clearCsrfToken } from '$lib/features/access/session/api';
import { captureRequests, jsonResponse } from '$lib/api/test/requestCapture';
import {
  createPricingSource,
  publishPricingSnapshot,
  refreshPricingSource,
  updatePricingSource,
  type PricingSource,
  type PricingSourceSnapshot
} from '$lib/features/usage/pricingSources';

const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

const session = {
  user: {
    id: '01980000-0000-7000-8000-000000000401',
    email: 'operator@example.com',
    display_name: 'Operator',
    role: 'operator' as const,
    access_scope: 'global' as const
  },
  csrf_token: 'csrf-pricing-token'
};

const source = {
  id: '01980000-0000-7000-8000-000000000501',
  etag: '01980000-0000-7000-8000-000000000502'
} as PricingSource;

const snapshot = {
  id: '01980000-0000-7000-8000-000000000503'
} as PricingSourceSnapshot;

afterEach(async () => {
  await authLifecycle.principalInvalidated();
  clearCsrfToken();
  vi.unstubAllGlobals();
});

describe('pricing sources', () => {
  it('creates a source with an idempotency key', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() =>
      jsonResponse({ ...source, name: 'vendor list' })
    );

    await createPricingSource({
      name: 'vendor list',
      url: 'https://prices.example.com/list.json'
    });

    const request = requests[0]!;
    expect(request.method).toBe('POST');
    expect(new URL(request.url).pathname).toBe('/api/v3/pricing/sources');
    expect(request.headers.get('idempotency-key')).toMatch(uuid);
  });

  it('patches a source under its ETag', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() => jsonResponse(source));

    await updatePricingSource(source, { enabled: false });

    const request = requests[0]!;
    expect(request.method).toBe('PATCH');
    expect(new URL(request.url).pathname).toBe(
      `/api/v3/pricing/sources/${source.id}`
    );
    expect(request.headers.get('if-match')).toBe(`"${source.etag}"`);
  });

  it('refreshes a source without a body', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() =>
      jsonResponse({
        source,
        snapshot,
        diff: {
          added: [],
          added_count: 0,
          changed: [],
          changed_count: 0,
          removed: [],
          removed_count: 0
        }
      })
    );

    await refreshPricingSource(source);

    const request = requests[0]!;
    expect(request.method).toBe('POST');
    expect(new URL(request.url).pathname).toBe(
      `/api/v3/pricing/sources/${source.id}/refresh`
    );
    expect(await request.text()).toBe('');
  });

  it('publishes a snapshot with overrides under an idempotency key', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() =>
      jsonResponse({ id: '01980000-0000-7000-8000-000000000504', revision: 3 })
    );

    await publishPricingSnapshot(snapshot, {
      effective_at: '2026-07-13T00:00:00Z',
      overrides: [
        {
          provider_kind: 'openai',
          model: 'gpt-5',
          operation: 'generation',
          output_per_million: '9.50',
          currency: 'USD'
        }
      ]
    });

    const request = requests[0]!;
    expect(request.method).toBe('POST');
    expect(new URL(request.url).pathname).toBe(
      `/api/v3/pricing/source-snapshots/${snapshot.id}/publish`
    );
    expect(request.headers.get('idempotency-key')).toMatch(uuid);
    const body = (await request.json()) as {
      effective_at: string;
      overrides: unknown[];
    };
    expect(body.effective_at).toBe('2026-07-13T00:00:00Z');
    expect(body.overrides).toHaveLength(1);
  });
});
