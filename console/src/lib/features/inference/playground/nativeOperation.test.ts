import { afterEach, expect, it, vi } from 'vitest';
import { gatewayFetch } from '$lib/api/gateway';
import { runNativeOperation } from './nativeOperation';

vi.mock('$lib/api/gateway', () => ({ gatewayFetch: vi.fn() }));

afterEach(() => vi.clearAllMocks());

it.each([
  ['openai-decisions', '/v1/decisions'],
  ['systemone', '/v1/systemone']
])(
  'sends %s through its public endpoint for transformed routes',
  async (dialect, path) => {
    vi.mocked(gatewayFetch).mockResolvedValue(
      new Response('{"answers":[]}', {
        headers: { 'Content-Type': 'application/json' }
      })
    );
    await runNativeOperation(
      'decision',
      dialect,
      'decision-route',
      'olp_test',
      '{"model":"","questions":[]}'
    );
    expect(gatewayFetch).toHaveBeenCalledWith(
      path,
      expect.objectContaining({
        method: 'POST',
        body: '{"model":"decision-route","questions":[]}'
      })
    );
  }
);

it('binds the OCR body model to the route', async () => {
  vi.mocked(gatewayFetch).mockResolvedValue(
    new Response('{"pages":[]}', {
      headers: { 'Content-Type': 'application/json' }
    })
  );
  await runNativeOperation(
    'ocr',
    'mistral-ocr',
    'ocr-route',
    'olp_test',
    '{"model":"","document":{"type":"image_url","image_url":"data:image/png;base64,x"}}'
  );
  expect(gatewayFetch).toHaveBeenCalledWith(
    '/v1/ocr',
    expect.objectContaining({
      method: 'POST',
      body: '{"model":"ocr-route","document":{"type":"image_url","image_url":"data:image/png;base64,x"}}'
    })
  );
});

it('sends Perplexity search to /v1/search with the route header', async () => {
  vi.mocked(gatewayFetch).mockResolvedValue(
    new Response('{"id":"s","results":[]}', {
      headers: { 'Content-Type': 'application/json' }
    })
  );
  await runNativeOperation(
    'search',
    'perplexity-search',
    'search-route',
    'olp_test',
    '{"query":"q","max_results":3}'
  );
  const [path, init] = vi.mocked(gatewayFetch).mock.calls[0]!;
  expect(path).toBe('/v1/search');
  expect((init?.headers as Headers).get('X-OLP-Route')).toBe('search-route');
  expect(init?.body).toBe('{"query":"q","max_results":3}');
});

it.each(['tavily-search', 'exa-search', 'parallel-search'])(
  'sends %s through the native model path',
  async (dialect) => {
    vi.mocked(gatewayFetch).mockResolvedValue(
      new Response('{"results":[]}', {
        headers: { 'Content-Type': 'application/json' }
      })
    );
    await runNativeOperation(
      'search',
      dialect,
      'search-route',
      'olp_test',
      '{"query":"q"}'
    );
    expect(vi.mocked(gatewayFetch).mock.calls[0]![0]).toBe(
      `/native/${dialect}/models/search-route`
    );
  }
);

it.each([
  'perplexity-search',
  'tavily-search',
  'exa-search',
  'parallel-search'
])('refuses a body model for %s', async (dialect) => {
  await expect(
    runNativeOperation(
      'search',
      dialect,
      'search-route',
      'olp_test',
      '{"model":"search-route","query":"q"}'
    )
  ).rejects.toThrow('no model field');
});
