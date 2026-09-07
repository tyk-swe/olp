import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiProblem } from '$lib/api/http';
import { listUserPage } from '$lib/features/access/api';
import { listApiKeyPage } from '$lib/features/access/api-keys/api';
import { getOidcConfiguration } from '$lib/features/access/oidc/api';
import { listProviderPage, listProviders } from '$lib/features/providers/api';
import { listRouteDraftPage } from '$lib/features/routes/api';
import { collectCursorPages } from '$lib/api/pagination';
import { captureRequests, jsonResponse } from '$lib/api/test/requestCapture';

afterEach(() => {
  vi.unstubAllGlobals();
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
        new URL(request.url).pathname === '/api/v3/users'
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
      '/api/v3/providers',
      '/api/v3/route-drafts',
      '/api/v3/api-keys',
      '/api/v3/users',
      '/api/v3/oidc/configuration'
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
});
