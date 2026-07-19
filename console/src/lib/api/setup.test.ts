import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiProblem } from './http';
import { createOwner, getSetupStatus } from './setup';

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('setup API', () => {
  it('loads setup status without caching or cross-origin credentials', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ setup_required: true }), {
        status: 200,
        headers: { 'content-type': 'application/json' }
      })
    );
    vi.stubGlobal('fetch', fetchMock);

    await expect(getSetupStatus()).resolves.toEqual({ setup_required: true });
    expect(fetchMock).toHaveBeenCalledOnce();
    const request = fetchMock.mock.calls[0]?.[0] as Request;
    expect(new URL(request.url).pathname).toBe('/api/v1/setup/status');
    expect(request.cache).toBe('no-store');
    expect(request.credentials).toBe('same-origin');
    expect(request.redirect).toBe('error');
  });

  it('posts only the backend owner contract with an idempotency key', async () => {
    const responseBody = {
      user: {
        id: '01980000-0000-7000-8000-000000000001',
        email: 'owner@example.com',
        display_name: 'Ada Owner',
        role: 'owner'
      },
      csrf_token: 'csrf-value'
    };
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify(responseBody), {
        status: 201,
        headers: { 'content-type': 'application/json' }
      })
    );
    vi.stubGlobal('fetch', fetchMock);

    await expect(
      createOwner(
        { email: 'owner@example.com', password: 'long secure phrase', display_name: 'Ada Owner' },
        'idempotency-value',
        'bootstrap-token-value'
      )
    ).resolves.toEqual(responseBody);

    const request = fetchMock.mock.calls[0]?.[0] as Request;
    expect(new URL(request.url).pathname).toBe('/api/v1/setup');
    expect(request.method).toBe('POST');
    expect(await request.clone().text()).toBe(
      JSON.stringify({
        email: 'owner@example.com',
        password: 'long secure phrase',
        display_name: 'Ada Owner'
      })
    );
    expect(request.headers.get('idempotency-key')).toBe('idempotency-value');
    expect(request.headers.get('x-olp-setup-token')).toBe('bootstrap-token-value');
  });

  it('normalizes RFC 9457 field errors', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        new Response(
          JSON.stringify({
            type: 'urn:olp:problem:validation',
            title: 'Validation failed',
            status: 422,
            errors: { email: ['That address is already in use.'] }
          }),
          { status: 422, headers: { 'content-type': 'application/problem+json' } }
        )
      )
    );

    const error = await getSetupStatus().catch((value: unknown) => value);
    expect(error).toBeInstanceOf(ApiProblem);
    expect((error as ApiProblem).problem.errors).toEqual({
      email: ['That address is already in use.']
    });
  });
});
