import { describe, expect, it, vi } from 'vitest';
import { json, route, serve } from '#lib/session/test/transport.js';
import { listDefinitions } from './api.js';

describe('guardrail definition reads', () => {
  it('collects all paginated guardrail definition pages', async () => {
    const fetch = vi.fn(async (request: Request) => {
      route(request, 'GET', '/api/v1/guardrails');
      return new URL(request.url).searchParams.has('cursor')
        ? json({ items: [{ id: 'g-2' }], next_cursor: null })
        : json({ items: [{ id: 'g-1' }], next_cursor: 'page-2' });
    });
    serve(fetch);
    const result = await listDefinitions(new AbortController().signal);
    expect(result.map((item) => item.id)).toEqual(['g-1', 'g-2']);
    expect(new URL(fetch.mock.calls[1][0].url).searchParams.get('cursor')).toBe(
      'page-2'
    );
  });

  it('forwards cancellation while paging guardrail definitions', async () => {
    const controller = new AbortController();
    serve(async (request) => {
      route(request, 'GET', '/api/v1/guardrails');
      if (new URL(request.url).searchParams.has('cursor')) {
        expect(request.signal.aborted).toBe(true);
        throw new DOMException('Request aborted', 'AbortError');
      }
      controller.abort();
      return json({ items: [{ id: 'g-1' }], next_cursor: 'page-2' });
    });
    await expect(listDefinitions(controller.signal)).rejects.toMatchObject({
      name: 'AbortError'
    });
  });
});
