import { describe, expect, it, vi } from 'vitest';
import { createManagementClient, requestNative } from './transport';

const passthroughAuth = {
  onRequest({ request }: { request: Request }) {
    return request;
  }
};

describe('createManagementClient', () => {
  it('sets accept, quotes if-match, invokes auth middleware, and serializes JSON vs FormData', async () => {
    const seen: {
      accept: string | null;
      ifMatch: string | null;
      auth: string | null;
      body: string;
    }[] = [];
    const client = createManagementClient({
      origin: 'http://console.test',
      fetch: async (request) => {
        seen.push({
          accept: request.headers.get('accept'),
          ifMatch: request.headers.get('if-match'),
          auth: request.headers.get('x-auth'),
          body: await request.text()
        });
        return new Response(JSON.stringify({ ok: true }), {
          status: 200,
          headers: { 'content-type': 'application/json' }
        });
      },
      authentication: {
        onRequest({ request }) {
          request.headers.set('x-auth', 'applied');
          return request;
        }
      }
    });

    await client.request('put', '/api/v1/settings/{key}', {
      params: {
        path: { key: 'branding' },
        header: { 'Idempotency-Key': 'req-1', 'If-Match': 'etag-42' }
      },
      body: { value: 'dark' } as never
    });

    expect(seen).toHaveLength(1);
    expect(seen[0]).toMatchObject({
      accept: 'application/json',
      ifMatch: '"etag-42"',
      auth: 'applied',
      body: '{"value":"dark"}'
    });

    const form = new FormData();
    form.set('file', 'content');
    const fetchSpy = vi.fn(
      async (request: Request) =>
        new Response(JSON.stringify({ ok: true }), {
          status: 200,
          headers: {
            'content-type': 'application/json',
            'x-seen-ct': request.headers.get('content-type') ?? ''
          }
        })
    );
    const formClient = createManagementClient({
      origin: 'http://console.test',
      fetch: fetchSpy,
      authentication: passthroughAuth
    });
    await formClient.request('post', '/api/v1/providers', {
      params: { header: { 'Idempotency-Key': 'req-form' } },
      body: form as never
    });
    const sent = fetchSpy.mock.calls[0]![0];
    expect(sent.headers.get('content-type')).toContain('multipart/form-data');
  });
});

describe('requestNative', () => {
  it('parses native JSON responses and rejects invalid JSON payloads', async () => {
    const validClient = createManagementClient({
      origin: 'http://console.test',
      fetch: async () =>
        new Response('{"items":[],"total":1}', {
          status: 200,
          headers: { 'content-type': 'application/json' }
        }),
      authentication: passthroughAuth
    });
    const data = await requestNative(
      validClient,
      'get',
      '/api/v1/providers',
      {},
      []
    );
    expect(data).toEqual({ items: [], total: 1 });

    const brokenClient = createManagementClient({
      origin: 'http://console.test',
      fetch: async () =>
        new Response('{not-json', {
          status: 200,
          headers: { 'content-type': 'application/json' }
        }),
      authentication: passthroughAuth
    });
    await expect(
      requestNative(brokenClient, 'get', '/api/v1/providers', {}, [])
    ).rejects.toMatchObject({
      problem: {
        status: 502,
        title: 'The API response is not valid JSON'
      }
    });
  });

  it('propagates management problem responses without attempting native parsing', async () => {
    const problemClient = createManagementClient({
      origin: 'http://console.test',
      fetch: async () =>
        new Response(
          JSON.stringify({
            type: 'urn:olp:problem:forbidden',
            title: 'Forbidden',
            status: 403
          }),
          {
            status: 403,
            headers: { 'content-type': 'application/problem+json' }
          }
        ),
      authentication: passthroughAuth
    });
    await expect(
      requestNative(problemClient, 'get', '/api/v1/providers', {}, [])
    ).rejects.toMatchObject({
      problem: { status: 403, title: 'Forbidden' }
    });
  });
});
