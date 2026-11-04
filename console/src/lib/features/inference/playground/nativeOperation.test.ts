import { afterEach, expect, it, vi } from 'vitest';
import { gatewayFetch } from '$lib/management/gateway';
import { stringifyNativeJSON } from '$lib/nativeJson';
import { translateAudio } from './audioTranslation';
import { runNativeOperation } from './nativeOperation';

vi.mock('$lib/management/gateway', () => ({ gatewayFetch: vi.fn() }));

afterEach(() => vi.clearAllMocks());

it.each([
  { name: 'an unregistered dialect', dialect: 'tei-embeddings' },
  { name: 'an invalid route', route: '../other' },
  { name: 'an invalid key', key: 'invalid' },
  { name: 'a non-object request', source: '[]' },
  { name: 'a different body route', source: '{"model":"other"}' },
  { name: 'a streaming request', source: '{"stream":true}' },
  {
    name: 'an oversized request',
    source: JSON.stringify({ prompt: 'é'.repeat(1 << 19) })
  }
])('refuses $name before dispatch', async (input) => {
  await expect(
    runNativeOperation(
      'completions',
      input.dialect ?? 'openai-completions',
      input.route ?? 'chosen-route',
      input.key ?? 'olp_test',
      input.source ?? '{}'
    )
  ).rejects.toThrow();
  expect(gatewayFetch).not.toHaveBeenCalled();
});

it('binds a completions request to the selected route and preserves its result', async () => {
  const raw = '{"id":"result","count":9007199254740993}';
  vi.mocked(gatewayFetch).mockResolvedValue(new Response(raw));

  const result = await runNativeOperation(
    'completions',
    'openai-completions',
    'chosen-route',
    'olp_test',
    '{"model":"","prompt":"hello"}'
  );

  const [path, options] = vi.mocked(gatewayFetch).mock.calls[0];
  const headers = new Headers(options?.headers);
  expect(path).toBe('/v1/completions');
  expect(options?.method).toBe('POST');
  expect(headers.get('Authorization')).toBe('Bearer olp_test');
  expect(headers.get('Cache-Control')).toBe('no-store');
  expect(JSON.parse(String(options?.body))).toEqual({
    model: 'chosen-route',
    prompt: 'hello'
  });
  expect(result.raw).toBe(raw);
  expect(stringifyNativeJSON(result.response)).toBe(raw);
});

it('sends URL-bound embeddings with the raw-vector contract', async () => {
  vi.mocked(gatewayFetch).mockResolvedValue(new Response('{}'));
  await runNativeOperation(
    'embeddings',
    'tei-embeddings',
    'chosen-route',
    'olp_test',
    '{"inputs":["hello"]}'
  );

  const [path, options] = vi.mocked(gatewayFetch).mock.calls[0];
  expect(path).toBe('/native/tei-embeddings/models/chosen-route');
  expect(new Headers(options?.headers).get('X-OLP-Client-Contract')).toBe(
    'raw-vector-storage/1'
  );
  expect(JSON.parse(String(options?.body))).toEqual({ inputs: ['hello'] });

  await expect(
    runNativeOperation(
      'embeddings',
      'tei-embeddings',
      'chosen-route',
      'olp_test',
      '{"model":""}'
    )
  ).rejects.toThrow();
  expect(gatewayFetch).toHaveBeenCalledOnce();
});

it.each([
  { response: 'denied', status: 403, error: /not admitted \(403\)/ },
  { response: 'not json', status: 200, error: /invalid native JSON result/ }
])('reports unsuccessful or invalid native responses', async (input) => {
  vi.mocked(gatewayFetch).mockResolvedValue(
    new Response(input.response, { status: input.status })
  );
  await expect(
    runNativeOperation(
      'completions',
      'openai-completions',
      'chosen-route',
      'olp_test',
      '{}'
    )
  ).rejects.toThrow(input.error);
  expect(gatewayFetch).toHaveBeenCalledOnce();
});

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
