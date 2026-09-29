import { afterEach, describe, expect, it, vi } from 'vitest';
import { listAudit } from '$lib/features/access/audit/api';
import { listApiKeyPage } from '$lib/features/access/api-keys/api';
import { getOidcConfiguration } from '$lib/features/access/oidc/api';
import { listUserPage, listUsers } from '$lib/features/access/users/api';
import {
  listProviderPage,
  listProviders
} from '$lib/features/providers/api/providers';
import { listRouteDraftPage } from '$lib/features/routes/api';
import { collectCursorPages } from '$lib/api/pagination';
import {
  listProviderHealth,
  listRequestMetadataGatewayEpochs
} from '$lib/features/runtime/health/api';
import { ApiProblem } from '$lib/api/http';
import { listMediaJobs } from '$lib/features/media/api';
import { listPricing } from '$lib/features/usage/api/pricing';
import { listRequests } from '$lib/features/usage/history/api';
import { listRuntimeGenerations } from '$lib/features/runtime/api';
import { captureRequests, jsonResponse } from '$lib/api/test/requestCapture';

afterEach(() => {
  vi.unstubAllGlobals();
});

function providerHealth(providerId: string) {
  return {
    provider_id: providerId,
    provider_name: `Provider ${providerId}`,
    provider_kind: 'openai',
    provider_state: 'active',
    status: 'healthy',
    attempt_count: 1,
    success_count: 1,
    rate_limit_count: 0,
    server_error_count: 0,
    transport_error_count: 0
  };
}

describe('request query serialization', () => {
  it('omits empty filters while retaining numeric zero', async () => {
    const requests = captureRequests(() =>
      jsonResponse({ items: [], next_cursor: null })
    );

    await listRequests({
      limit: 25,
      route: '',
      provider_id: undefined,
      status_code: 0
    });

    expect(Object.fromEntries(new URL(requests[0]!.url).searchParams)).toEqual({
      limit: '25',
      status_code: '0'
    });
  });
});

describe('resource API cursor pages', () => {
  it('normalizes every paginated response at the API boundary', async () => {
    captureRequests((_request, index) =>
      jsonResponse({
        items: [{ index }],
        next_cursor: index === 5 ? null : `page-${index + 2}`
      })
    );

    const pages = [
      await listRequests({}),
      await listMediaJobs({}),
      await listRequestMetadataGatewayEpochs('unresolved'),
      await listAudit(),
      await listRuntimeGenerations(),
      await listPricing()
    ];

    pages.forEach((page, index) => {
      expect(page).toEqual({
        items: [{ index }],
        nextCursor: index === 5 ? null : `page-${index + 2}`
      });
    });
  });
});

describe('audit filters', () => {
  it('sends every filled filter and omits the empty ones', async () => {
    const requests = captureRequests(() =>
      jsonResponse({ items: [], next_cursor: null })
    );

    await listAudit({
      action: 'provider.update',
      resource_type: 'provider',
      resource_id: '01980000-0000-7000-8000-000000000104',
      actor_user_id: '01980000-0000-7000-8000-000000000001',
      outcome: 'failure',
      occurred_after: '2026-07-12T09:30:00.000Z',
      occurred_before: undefined,
      cursor: 'audit-next'
    });

    const query = new URL(requests[0].url).searchParams;
    expect(Object.fromEntries(query)).toEqual({
      limit: '50',
      action: 'provider.update',
      resource_type: 'provider',
      resource_id: '01980000-0000-7000-8000-000000000104',
      actor_user_id: '01980000-0000-7000-8000-000000000001',
      outcome: 'failure',
      occurred_after: '2026-07-12T09:30:00.000Z',
      cursor: 'audit-next'
    });
  });
});

describe('provider-health pagination', () => {
  it('aggregates every page and sends each cursor once', async () => {
    const first = providerHealth('provider-1');
    const second = providerHealth('provider-2');
    const requests = captureRequests((_request, index) =>
      index === 0
        ? jsonResponse({
            window_minutes: 30,
            items: [first],
            next_cursor: 'page-2'
          })
        : jsonResponse({
            window_minutes: 30,
            items: [second],
            next_cursor: null
          })
    );

    await expect(listProviderHealth(30)).resolves.toEqual({
      window_minutes: 30,
      items: [first, second]
    });
    expect(requests).toHaveLength(2);
    expect(new URL(requests[0]!.url).searchParams.get('cursor')).toBeNull();
    expect(new URL(requests[1]!.url).searchParams.get('cursor')).toBe('page-2');
    for (const request of requests) {
      const params = new URL(request.url).searchParams;
      expect(params.get('window_minutes')).toBe('30');
      expect(params.get('limit')).toBe('200');
    }
  });

  it('rejects a repeated cursor instead of looping', async () => {
    const requests = captureRequests(() =>
      jsonResponse({ window_minutes: 15, items: [], next_cursor: 'repeat' })
    );

    const error = await listProviderHealth().catch((value: unknown) => value);

    expect(error).toBeInstanceOf(ApiProblem);
    expect((error as ApiProblem).problem).toEqual({
      type: 'urn:olp:problem:invalid-cursor-cycle',
      title: 'The control API returned a repeated pagination cursor',
      status: 502
    });
    expect(requests).toHaveLength(2);
  });

  it('enforces the shared collection safety limit', async () => {
    const requests = captureRequests((_request, page) =>
      jsonResponse({
        window_minutes: 15,
        items: Array.from({ length: 200 }, (_, item) =>
          providerHealth(`provider-${page + 1}-${item + 1}`)
        ),
        next_cursor: `page-${page + 2}`
      })
    );

    const error = await listProviderHealth().catch((value: unknown) => value);

    expect(error).toBeInstanceOf(ApiProblem);
    expect((error as ApiProblem).problem).toEqual({
      type: 'urn:olp:problem:pagination-limit-exceeded',
      title: 'The control API collection exceeds the console safety limit',
      status: 502
    });
    expect(requests).toHaveLength(51);
  });
});

describe('resource API errors', () => {
  it('preserves structured problem details', async () => {
    captureRequests(() =>
      jsonResponse(
        {
          type: 'urn:olp:problem:rate-limited',
          title: 'Rate limited',
          detail: 'Retry after the window',
          status: 429,
          instance: '/api/v1/requests',
          errors: {
            request: [
              { code: 'invalid', message: 'Retry after the advertised window.' }
            ]
          }
        },
        { status: 503, headers: { 'content-type': 'application/problem+json' } }
      )
    );

    const error = await listRequests({}).catch((value: unknown) => value);

    expect(error).toBeInstanceOf(ApiProblem);
    expect((error as ApiProblem).problem).toEqual({
      type: 'urn:olp:problem:rate-limited',
      title: 'Rate limited',
      detail: 'Retry after the window',
      status: 429,
      instance: '/api/v1/requests',
      errors: {
        request: [
          { code: 'invalid', message: 'Retry after the advertised window.' }
        ]
      }
    });
  });

  it('fails closed when a successful response omits its required JSON body', async () => {
    captureRequests(
      () =>
        new Response(null, { status: 200, headers: { 'content-length': '0' } })
    );

    const error = await listRequests({}).catch((value: unknown) => value);

    expect(error).toBeInstanceOf(ApiProblem);
    expect((error as ApiProblem).problem).toEqual({
      type: 'urn:olp:problem:invalid-api-response',
      title: 'The API response did not include the expected JSON body',
      status: 502
    });
  });

  it('fails closed when a successful response returns null instead of its required object', async () => {
    captureRequests(() => jsonResponse(null));

    const error = await listRequests({}).catch((value: unknown) => value);

    expect(error).toBeInstanceOf(ApiProblem);
    expect((error as ApiProblem).problem).toEqual({
      type: 'urn:olp:problem:invalid-api-response',
      title: 'The API response did not include the expected JSON body',
      status: 502
    });
  });

  it('falls back to the response status for unstructured errors', async () => {
    captureRequests(() => jsonResponse('gateway unavailable', { status: 503 }));

    const error = await listRequests({}).catch((value: unknown) => value);

    expect(error).toBeInstanceOf(ApiProblem);
    expect((error as ApiProblem).problem).toEqual({
      type: 'about:blank',
      title: 'Request failed (503)',
      status: 503
    });
  });
});

describe('management selector pagination', () => {
  it('collects every cursor page for route, provider, and checklist selectors', async () => {
    const seen: Array<string | undefined> = [];
    const items = await collectCursorPages(async (cursor?: string) => {
      seen.push(cursor);
      if (!cursor) return { items: ['provider-1'], nextCursor: 'page-2' };
      return { items: ['provider-2'], nextCursor: null };
    });

    expect(items).toEqual(['provider-1', 'provider-2']);
    expect(seen).toEqual([undefined, 'page-2']);
  });

  it('fails closed if the API repeats a cursor', async () => {
    await expect(
      collectCursorPages(async () => ({ items: [], nextCursor: 'repeat' }))
    ).rejects.toBeInstanceOf(ApiProblem);
  });
});

describe('management resources', () => {
  it('forwards abort signals to resource reads', async () => {
    const controller = new AbortController();
    const requests = captureRequests((request) => {
      const body =
        new URL(request.url).pathname === '/api/v1/users'
          ? { items: [], next_cursor: null }
          : { items: [], next_cursor: null };
      return jsonResponse(body);
    });

    await listProviderPage(undefined, controller.signal);
    await listRouteDraftPage(undefined, controller.signal);
    await listApiKeyPage(undefined, controller.signal);
    await listUserPage(undefined, controller.signal);
    await getOidcConfiguration(controller.signal);

    expect(requests.map((request) => new URL(request.url).pathname)).toEqual([
      '/api/v1/providers',
      '/api/v1/route-drafts',
      '/api/v1/api-keys',
      '/api/v1/users',
      '/api/v1/oidc/configuration'
    ]);
    expect(requests.every((request) => !request.signal.aborted)).toBe(true);

    controller.abort();

    expect(requests.every((request) => request.signal.aborted)).toBe(true);
  });

  it('forwards abort signals across every cursor page', async () => {
    const controller = new AbortController();
    const requests = captureRequests((_request, index) =>
      jsonResponse(
        index === 0
          ? { items: ['provider-1'], next_cursor: 'page-2' }
          : { items: ['provider-2'], next_cursor: null }
      )
    );

    await expect(listProviders(controller.signal)).resolves.toEqual([
      'provider-1',
      'provider-2'
    ]);
    expect(requests).toHaveLength(2);
    expect(new URL(requests[0]!.url).searchParams.get('cursor')).toBeNull();
    expect(new URL(requests[1]!.url).searchParams.get('cursor')).toBe('page-2');

    controller.abort();

    expect(requests.every((request) => request.signal.aborted)).toBe(true);
  });

  it('collects every user page for full-roster selectors', async () => {
    const controller = new AbortController();
    const requests = captureRequests((_request, index) =>
      jsonResponse(
        index === 0
          ? { items: [{ id: 'user-1' }], next_cursor: 'page-2' }
          : { items: [{ id: 'user-2' }], next_cursor: null }
      )
    );

    await expect(listUsers(controller.signal)).resolves.toEqual([
      { id: 'user-1' },
      { id: 'user-2' }
    ]);
    expect(requests).toHaveLength(2);
    expect(new URL(requests[0]!.url).searchParams.get('cursor')).toBeNull();
    expect(new URL(requests[1]!.url).searchParams.get('cursor')).toBe('page-2');

    controller.abort();

    expect(requests.every((request) => request.signal.aborted)).toBe(true);
  });
});
