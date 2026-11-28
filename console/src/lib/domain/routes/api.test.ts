import { describe, expect, it, vi } from 'vitest';
import { json, route, serve } from '#lib/session/test/transport.js';
import { listRoutes } from './api.js';

describe('route reads', () => {
  it('loads every page of published routes', async () => {
    const cursors = vi.fn<(cursor: string | null) => void>();
    serve(
      (request) => {
        route(request, 'GET', '/api/v1/routes');
        const query = new URL(request.url).searchParams;
        expect(query.get('limit')).toBe('50');
        cursors(query.get('cursor'));
        return json({
          items: [],
          next_cursor: query.has('cursor') ? null : 'next-page'
        });
      },
      ['read']
    );

    await expect(listRoutes()).resolves.toEqual([]);
    expect(cursors.mock.calls).toEqual([[null], ['next-page']]);
  });
});
