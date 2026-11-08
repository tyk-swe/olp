import { describe, expect, it, vi } from 'vitest';
import { json, route, serve } from '#lib/management/test/transport.js';
import {
  createApiKey,
  getApiKey,
  listApiKeyPage,
  listApiKeys,
  revokeApiKey,
  rotateApiKey,
  updateApiKey,
  type ApiKey
} from './api';

const key: ApiKey = {
  id: 'key-id',
  lookup_id: 'lookup-id',
  name: 'Service',
  etag: '018f1234-5678-7abc-8def-0123456789ab',
  project_id: null,
  project_name: null,
  budget_group_id: null,
  created_by: 'owner',
  created_by_email: 'owner@example.test',
  created_at: '2025-01-01T00:00:00Z',
  allow_provider_state: false,
  allowed_attribution_keys: [],
  allowed_routes: [],
  response_metadata: false,
  regional_limits: {},
  scopes: [],
  budget: {
    daily: {
      accrued: '0',
      limit: null,
      window_ends_at: '2025-01-02T00:00:00Z'
    },
    monthly: {
      accrued: '0',
      limit: null,
      window_ends_at: '2025-02-01T00:00:00Z'
    },
    unpriced_attempts: 0
  }
};
const runtimeGeneration = { id: 'generation-id', sequence: 1 };
const secret = {
  id: key.id,
  lookup_id: key.lookup_id,
  secret: 'one-time-secret',
  runtime_generation: runtimeGeneration
};

describe('API key management', () => {
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

  it('creates a key using the supplied request identity', async () => {
    const input = { name: key.name, allowed_routes: ['route-a'] };
    serve(async (request) => {
      route(request, 'POST', '/api/v1/api-keys');
      expect(request.headers.get('Idempotency-Key')).toBe('create-request');
      expect(await request.json()).toEqual(input);
      return json(secret);
    });

    expect(await createApiKey(input, 'create-request')).toEqual(secret);
  });

  it('updates a key using its quoted revision', async () => {
    const input = { name: 'Renamed' };
    const updated = {
      etag: 'next-version',
      runtime_generation: runtimeGeneration
    };
    serve(async (request) => {
      route(request, 'PATCH', `/api/v1/api-keys/${key.id}`);
      expect(request.headers.get('If-Match')).toBe(`"${key.etag}"`);
      expect(await request.json()).toEqual(input);
      return json(updated);
    });

    expect(await updateApiKey(key, input)).toEqual(updated);
  });

  it('rotates a key using its quoted revision and request identity', async () => {
    const rotated = { ...secret, etag: 'next-version' };
    serve(async (request) => {
      route(request, 'POST', `/api/v1/api-keys/${key.id}/rotate`);
      expect(request.headers.get('If-Match')).toBe(`"${key.etag}"`);
      expect(request.headers.get('Idempotency-Key')).toBe('rotate-request');
      expect(await request.json()).toEqual({ overlap_seconds: 60 });
      return json(rotated);
    });

    expect(await rotateApiKey(key, 60, 'rotate-request')).toEqual(rotated);
  });

  it('revokes a key using its quoted revision and request identity', async () => {
    serve((request) => {
      route(request, 'POST', `/api/v1/api-keys/${key.id}/revoke`);
      expect(request.headers.get('If-Match')).toBe(`"${key.etag}"`);
      expect(request.headers.get('Idempotency-Key')).toBe('revoke-request');
      return json({
        etag: 'next-version',
        runtime_generation: runtimeGeneration
      });
    });

    await expect(revokeApiKey(key, 'revoke-request')).resolves.toBeUndefined();
  });
});
