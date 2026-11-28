import { describe, expect, it, vi } from 'vitest';
import { json, route, serve } from '#lib/session/test/transport.js';
import { getApiKey, listApiKeyPage, listApiKeys } from './api.js';

const key = { id: 'key-id', name: 'Service' };

describe('API key reads', () => {
  it('collects key pages and loads key details', async () => {
    const cursors = vi.fn<(cursor: string | null) => void>();
    serve((request) => {
      const url = new URL(request.url);
      if (url.pathname.endsWith(key.id)) {
        route(request, 'GET', `/api/v1/api-keys/${key.id}`);
        return json(key);
      }
      route(request, 'GET', '/api/v1/api-keys');
      const cursor = url.searchParams.get('cursor');
      cursors(cursor);
      expect(url.searchParams.get('limit')).toBe('50');
      return json(
        cursor
          ? { items: [{ ...key, id: 'second' }] }
          : { items: [key], next_cursor: 'next' }
      );
    });

    expect(await listApiKeys()).toEqual([key, { ...key, id: 'second' }]);
    expect(cursors.mock.calls).toEqual([[null], ['next']]);
    expect(await getApiKey(key.id)).toEqual(key);
  });

  it('passes a creator filter when loading a key page', async () => {
    serve((request) => {
      route(request, 'GET', '/api/v1/api-keys');
      const query = new URL(request.url).searchParams;
      expect(query.get('created_by')).toBe('owner');
      expect(query.get('cursor')).toBe('next');
      return json({ items: [key] });
    });

    expect((await listApiKeyPage('next', undefined, 'owner')).items).toEqual([
      key
    ]);
  });
});
