import { afterEach, expect, it, vi } from 'vitest';
import { gatewayFetch } from '$lib/api/gateway';
import { translateAudio } from './audioTranslation';
import { runNativeOperation } from './nativeOperation';

vi.mock('$lib/api/gateway', () => ({ gatewayFetch: vi.fn() }));

afterEach(() => vi.clearAllMocks());

it('enforces the published route length for native and audio requests', async () => {
  const route = 'a'.repeat(101);
  const validRoute = 'a'.repeat(100);
  const file = new File(['audio'], 'audio.wav', { type: 'audio/wav' });
  await expect(
    runNativeOperation(
      'ocr',
      'azure-document-intelligence-ocr',
      route,
      'olp_test',
      '{"model":"","base64Source":"QUJD"}'
    )
  ).rejects.toThrow('Choose an active published route slug.');
  await expect(
    translateAudio(route, 'olp_test', file, '', '', 'json')
  ).rejects.toThrow('Choose an active published route slug.');
  expect(gatewayFetch).not.toHaveBeenCalled();
  vi.mocked(gatewayFetch)
    .mockResolvedValueOnce(new Response('{}'))
    .mockResolvedValueOnce(new Response('{}'));
  const native = await runNativeOperation(
    'ocr',
    'azure-document-intelligence-ocr',
    validRoute,
    'olp_test',
    '{"model":"","base64Source":"QUJD"}'
  );
  expect(native.request).toMatchObject({ model: validRoute });
  await expect(
    translateAudio(validRoute, 'olp_test', file, '', '', 'json')
  ).resolves.toBe('{}');
  expect(gatewayFetch).toHaveBeenCalledTimes(2);
});

it.each([
  'perplexity-search',
  'tavily-search',
  'exa-search',
  'parallel-search',
  'brave-search'
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
