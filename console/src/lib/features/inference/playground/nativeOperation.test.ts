import { afterEach, expect, it, vi } from 'vitest';
import { runNativeOperation } from './nativeOperation';

vi.mock('$lib/api/gateway', () => ({ gatewayFetch: vi.fn() }));

afterEach(() => vi.clearAllMocks());

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

it('binds the route model and posts azure ocr to its native path', async () => {
  const { gatewayFetch } = await import('$lib/api/gateway');
  vi.mocked(gatewayFetch).mockResolvedValue(
    new Response('{"status":"succeeded"}', {
      status: 200,
      headers: { 'Content-Type': 'application/json' }
    })
  );
  const result = await runNativeOperation(
    'ocr',
    'azure-document-intelligence-ocr',
    'ocr-route',
    'olp_test',
    '{"model":"","base64Source":"QUJD"}'
  );
  expect(result.request).toMatchObject({ model: 'ocr-route' });
  expect(vi.mocked(gatewayFetch).mock.calls[0][0]).toBe(
    '/native/azure-document-intelligence-ocr/models/ocr-route'
  );
});
