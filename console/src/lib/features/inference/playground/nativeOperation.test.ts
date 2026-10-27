import { afterEach, expect, it, vi } from 'vitest';
import { gatewayFetch } from '$lib/api/gateway';
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

it('sends Vertex document processing to its native route path', async () => {
  vi.mocked(gatewayFetch).mockResolvedValue(
    new Response('{"document":{"text":"x","pages":[]}}', { status: 200 })
  );
  await runNativeOperation(
    'ocr',
    'vertex-document-processing-ocr',
    'ocr-route',
    'olp_test',
    '{"rawDocument":{"content":"QUJD","mimeType":"image/png"}}'
  );
  expect(gatewayFetch).toHaveBeenCalledWith(
    '/native/vertex-document-processing-ocr/models/ocr-route',
    expect.objectContaining({ method: 'POST' })
  );
});
