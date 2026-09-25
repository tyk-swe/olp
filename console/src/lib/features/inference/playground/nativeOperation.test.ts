import { afterEach, describe, expect, it, vi } from 'vitest';
import { stringifyNativeJSON } from '$lib/json/nativeJson';
import {
  nativeOperationRequest,
  runNativeOperation,
  type OperationDialect
} from './nativeOperation';

const dialect = (
  id: string,
  operation: string,
  model_binding: OperationDialect['model_binding']
): OperationDialect => ({
  id,
  operation,
  model_binding,
  revision: 'v1',
  operation_revision: 'v1',
  surface: 'native',
  mode: 'unary',
  label: id,
  documentation: '',
  evidence: ''
});

const catalog = [
  dialect('voyage-embeddings', 'embeddings', 'required'),
  dialect('tei-sparse-embeddings', 'embeddings', 'none'),
  dialect('tei-rerank', 'rerank', 'none')
];

afterEach(() => vi.restoreAllMocks());

describe('registered native operation browser client', () => {
  it('keeps packed storage and exact numeric metadata on the same-origin public path', async () => {
    const raw =
      '{"data":[{"index":0,"embedding":"AP8="}],"metadata":{"unsafe":9007199254740993}}';
    const transport = vi.spyOn(globalThis, 'fetch').mockResolvedValueOnce(
      new Response(raw, {
        status: 200,
        headers: { 'content-type': 'application/json' }
      })
    );
    const result = await runNativeOperation(
      catalog,
      'embeddings',
      'voyage-embeddings',
      'vector-route',
      'olp_secret',
      '{"model":"","input":"one","output_dtype":"ubinary","output_dimension":16,"seed":9007199254740993}'
    );
    expect(transport.mock.calls[0]![0]).toBe(
      '/native/voyage-embeddings/models/vector-route'
    );
    const sent = transport.mock.calls[0]![1]!;
    expect(sent.credentials).toBe('omit');
    expect((sent.headers as Headers).get('Authorization')).toBe(
      'Bearer olp_secret'
    );
    expect((sent.headers as Headers).get('X-OLP-Client-Contract')).toBe(
      'raw-vector-storage/1'
    );
    expect(sent.body).toContain('"model":"vector-route"');
    expect(sent.body).toContain('"seed":9007199254740993');
    expect(result.raw).toBe(raw);
    expect(stringifyNativeJSON(result.response)).toBe(raw);
  });

  it('does not add a model to URL-bound TEI requests or dispatch unknown dialects', async () => {
    expect(
      stringifyNativeJSON(
        nativeOperationRequest('{"inputs":"hello"}', 'route', 'none')
      )
    ).toBe('{"inputs":"hello"}');
    const transport = vi.spyOn(globalThis, 'fetch');
    await expect(
      runNativeOperation(
        catalog,
        'embeddings',
        'unregistered-dialect',
        'route',
        'olp_secret',
        '{}'
      )
    ).rejects.toThrow(/registered native dialect/);
    await expect(
      runNativeOperation(
        catalog,
        'rerank',
        'tei-rerank',
        'route',
        'wrong-key',
        '{}'
      )
    ).rejects.toThrow(/inference API key/);
    expect(transport).not.toHaveBeenCalled();
  });

  it('rejects a conflicting body model for a required binding', () => {
    expect(() =>
      nativeOperationRequest(
        '{"model":"other-route","input":"one"}',
        'route',
        'required'
      )
    ).toThrow(/another route/);
    expect(
      stringifyNativeJSON(
        nativeOperationRequest(
          '{"model":"","input":"one"}',
          'route',
          'required'
        )
      )
    ).toContain('"model":"route"');
  });
});
