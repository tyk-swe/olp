import { afterEach, describe, expect, it, vi } from 'vitest';
import { json, route, serve } from '#lib/management/test/transport.js';
import {
  createRouteDraft,
  listRouteDraftPage,
  listRouteRevisions,
  listRoutes,
  simulateRouting,
  type CreateRouteDraftInput,
  type RouteDraftValidation
} from './api';

afterEach(() => vi.unstubAllGlobals());

describe('route management API', () => {
  it('loads a draft page using its cursor and exposes the next page', async () => {
    serve((request) => {
      route(request, 'GET', '/api/v1/route-drafts');
      const url = new URL(request.url);
      expect(url.searchParams.get('cursor')).toBe('current');
      expect(url.searchParams.get('limit')).toBe('50');
      return json({ items: [], next_cursor: 'next' });
    });
    await expect(listRouteDraftPage('current')).resolves.toEqual({
      items: [],
      nextCursor: 'next'
    });
  });

  it.each([
    { path: '/api/v1/routes', limit: '50', load: () => listRoutes() },
    {
      path: '/api/v1/routes/route-id/revisions',
      limit: '100',
      load: () => listRouteRevisions('route-id')
    }
  ])('loads all pages from $path', async ({ path, limit, load }) => {
    const cursors = vi.fn<(cursor: string | null) => void>();
    serve((request) => {
      route(request, 'GET', path);
      const query = new URL(request.url).searchParams;
      expect(query.get('limit')).toBe(limit);
      cursors(query.get('cursor'));
      return json({
        items: [],
        next_cursor: query.has('cursor') ? null : 'next-page'
      });
    });
    await expect(load()).resolves.toEqual([]);
    expect(cursors.mock.calls).toEqual([[null], ['next-page']]);
  });

  it('creates a draft using its idempotency key and configuration', async () => {
    const input: CreateRouteDraftInput = {
      slug: 'summarize',
      project_id: null,
      targets: [
        {
          provider_id: '5a5c49e9-34d2-4d39-b946-cab14d70f1b1',
          provider_model: 'model-a',
          priority: 1,
          timeout_ms: 10000,
          weight: 1
        }
      ],
      operations: ['generation'],
      max_attempts: 1,
      overall_timeout_ms: 10000
    };
    const response: RouteDraftValidation = {
      id: '490213ec-ae6e-4a8c-bcf8-f4b906cf0167',
      etag: 'e702ac86-8a88-48c6-a582-2c0ecfc14b4e',
      project_id: null,
      slug: 'summarize',
      state: 'draft',
      fidelity: { mode: 'strict' }
    };
    serve(async (request) => {
      route(request, 'POST', '/api/v1/route-drafts');
      expect(request.headers.get('Idempotency-Key')).toBe('create-draft');
      expect(await request.json()).toEqual(input);
      return json(response);
    });
    await expect(createRouteDraft(input, 'create-draft')).resolves.toEqual(
      response
    );
  });

  it('sends generation context and cancellation to the routing simulator', async () => {
    const controller = new AbortController();
    serve(async (request) => {
      route(request, 'POST', '/api/v1/routing/simulate');
      expect(await request.json()).toEqual({
        operation: {
          operation: 'generation',
          request: {
            route: 'summarize',
            messages: [
              { role: 'user', content: [{ type: 'text', text: 'Hello' }] }
            ],
            parameters: {
              stream: false,
              temperature: 0.2,
              max_output_tokens: 120
            },
            tools: []
          }
        },
        surface: 'openai',
        mode: 'unary',
        api_key_id: 'key-id',
        seed: 'repeatable',
        estimated_input_tokens: 10,
        max_output_tokens: 200
      });
      controller.abort();
      expect(request.signal.aborted).toBe(true);
      return json([]);
    });
    await expect(
      simulateRouting(
        {
          route: 'summarize',
          surface: 'openai',
          mode: 'unary',
          temperature: 0.2,
          max_output_tokens: 120,
          apiKeyId: 'key-id',
          seed: 'repeatable',
          estimatedInputTokens: 10,
          maxOutputTokensOverride: 200
        },
        controller.signal
      )
    ).resolves.toEqual([]);
  });
});
