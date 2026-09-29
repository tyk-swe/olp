import { afterEach, describe, expect, it, vi } from 'vitest';
import { gatewayFetch } from '$lib/api/gateway';

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('gatewayFetch', () => {
  it('stays uncached, credential-free and redirect-denying', async () => {
    const transport = vi.fn<typeof fetch>(async () => new Response('{}'));
    vi.stubGlobal('fetch', transport);

    await gatewayFetch('/v1/responses', { method: 'POST' });

    const init = transport.mock.calls[0]![1]!;
    expect(init.cache).toBe('no-store');
    expect(init.credentials).toBe('omit');
    expect(init.redirect).toBe('error');
  });

  it('overrides conflicting caller options', async () => {
    const transport = vi.fn<typeof fetch>(async () => new Response('{}'));
    vi.stubGlobal('fetch', transport);

    await gatewayFetch('/v1/responses', {
      cache: 'force-cache',
      credentials: 'include',
      redirect: 'follow'
    });

    const init = transport.mock.calls[0]![1]!;
    expect(init.cache).toBe('no-store');
    expect(init.credentials).toBe('omit');
    expect(init.redirect).toBe('error');
  });
});
