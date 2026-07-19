import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiProblem } from './http';
import { listUserPage } from './management/access';
import { listApiKeyPage } from './management/api-keys';
import { getOidcConfiguration } from './management/oidc';
import { listProviderPage, listProviders } from './management/providers';
import { listRouteDraftPage } from './management/routes';
import { collectCursorPages } from './management/shared';
import { captureRequests, jsonResponse } from './test/requestCapture';

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
      const body = new URL(request.url).pathname === '/api/v1/users'
        ? { data: [], next_cursor: null }
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

    await expect(listProviders({ signal: controller.signal })).resolves.toEqual([
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
