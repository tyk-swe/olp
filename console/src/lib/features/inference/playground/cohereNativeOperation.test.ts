import { afterEach, expect, it, vi } from 'vitest';
import { parseNativeJSON, stringifyNativeJSON } from '$lib/json/nativeJson';
import {
  nativeDialects,
  nativeOperationRequest,
  runNativeOperation,
  type OperationDialect
} from './nativeOperation';
import { rerankRows, vectorRows } from './operationPresentation';
import { templateFor } from './templates';

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
  dialect('cohere-embed-v2', 'embeddings', 'required'),
  dialect('cohere-rerank-v2', 'rerank', 'required')
];

afterEach(() => vi.restoreAllMocks());

it('offers native Cohere v2 tasks and retains typed vector groups without float conversion', async () => {
  expect(nativeDialects(catalog, 'embeddings')).toContain('cohere-embed-v2');
  expect(nativeDialects(catalog, 'rerank')).toContain('cohere-rerank-v2');
  const template = templateFor('cohere-typed-embeddings');
  expect(template?.nativeDialect).toBe('cohere-embed-v2');
  expect(template?.request).toMatchObject({
    input_type: 'search_document',
    embedding_types: ['float', 'int8', 'ubinary']
  });
  const raw =
    '{"embeddings":{"float":[[0.10000000000000001,-0,1,2,3,4,5,6]],"int8":[[-128,127,0,1,2,3,4,5]],"ubinary":[[128]]},"meta":{"billed_units":{"input_tokens":2}},"opaque":{"counter":9007199254740993}}';
  const transport = vi.spyOn(globalThis, 'fetch').mockResolvedValueOnce(
    new Response(raw, {
      status: 200,
      headers: { 'content-type': 'application/json' }
    })
  );
  const source =
    '{"model":"","input_type":"search_document","texts":["one"],"embedding_types":["float","int8","ubinary"],"truncate":"NONE"}';
  const result = await runNativeOperation(
    catalog,
    'embeddings',
    'cohere-embed-v2',
    'cohere-route',
    'olp_secret',
    source
  );
  expect(transport.mock.calls[0]?.[0]).toBe(
    '/native/cohere-embed-v2/models/cohere-route'
  );
  const sent = transport.mock.calls[0]?.[1];
  expect((sent?.headers as Headers).get('X-OLP-Client-Contract')).toBe(
    'raw-vector-storage/1'
  );
  expect(sent?.body).toContain('"model":"cohere-route"');
  expect(result.raw).toBe(raw);
  expect(stringifyNativeJSON(result.response)).toBe(raw);
  expect(vectorRows(result.response, result.request)).toEqual([
    {
      position: 0,
      inputIndex: '0',
      layout: 'Dense array',
      dtype: 'float',
      logicalShape: '8',
      storageShape: '8 stored values'
    },
    {
      position: 1,
      inputIndex: '0',
      layout: 'Dense array',
      dtype: 'int8',
      logicalShape: '8',
      storageShape: '8 stored values'
    },
    {
      position: 2,
      inputIndex: '0',
      layout: 'Packed binary',
      dtype: 'ubinary',
      logicalShape: '8',
      storageShape: '1 stored byte'
    }
  ]);
  expect(
    vectorRows(parseNativeJSON('{"embeddings":{"base64":["AACAPwAAAMA="]}}'))[0]
  ).toMatchObject({
    dtype: 'base64',
    layout: 'Base64 storage',
    logicalShape: 'Unknown',
    storageShape: '8 stored bytes'
  });
});

it('keeps Cohere native rerank result order, ties, and score spelling', () => {
  const request = nativeOperationRequest(
    '{"model":"","query":"q","documents":["first","second"],"top_n":2}',
    'cohere-route',
    'required'
  );
  const response = parseNativeJSON(
    '{"results":[{"index":1,"relevance_score":0.1000000000000000000001},{"index":0,"relevance_score":0.1000000000000000000001}]}'
  );
  expect(rerankRows(response, request)).toEqual([
    {
      position: 0,
      inputIndex: '1',
      inputId: 'Not reported',
      score: '0.1000000000000000000001'
    },
    {
      position: 1,
      inputIndex: '0',
      inputId: 'Not reported',
      score: '0.1000000000000000000001'
    }
  ]);
});
