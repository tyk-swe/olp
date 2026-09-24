import { describe, expect, it } from 'vitest';
import { parseNativeJSON, stringifyNativeJSON } from '$lib/json/nativeJson';
import {
  nativeCountFields,
  rerankRows,
  vectorRows
} from './operationPresentation';

describe('native operation presentation', () => {
  it('separates packed and sparse logical shape from storage without float conversion', () => {
    const packed = parseNativeJSON(
      '{"data":[{"index":0,"embedding":"AP8="}],"provider_metadata":{"unsafe":9007199254740993}}'
    );
    expect(
      vectorRows(
        packed,
        parseNativeJSON('{"output_dtype":"ubinary","output_dimension":16}')
      )
    ).toEqual([
      {
        position: 0,
        inputIndex: '0',
        layout: 'Base64 storage',
        dtype: 'ubinary',
        logicalShape: '16',
        storageShape: '2 stored bytes'
      }
    ]);
    expect(stringifyNativeJSON(packed)).toContain('9007199254740993');
    const sparse = parseNativeJSON(
      '[[{"index":999,"value":-0},{"index":2,"value":1e-4}]]'
    );
    expect(vectorRows(sparse)[0]).toMatchObject({
      layout: 'Sparse coordinates',
      logicalShape: 'Unknown',
      storageShape: '2 index/value pairs'
    });
  });

  it('keeps rerank input IDs, native score spellings, order, and ties', () => {
    const request = parseNativeJSON(
      '{"documents":[{"id":"alpha","text":"a"},{"id":"beta","text":"b"}]}'
    );
    const response = parseNativeJSON(
      '{"results":[{"index":1,"relevance_score":0.1000000000000000000001},{"index":0,"relevance_score":0.1000000000000000000001}]}'
    );
    expect(rerankRows(response, request)).toEqual([
      {
        position: 0,
        inputIndex: '1',
        inputId: '"beta"',
        score: '0.1000000000000000000001'
      },
      {
        position: 1,
        inputIndex: '0',
        inputId: '"alpha"',
        score: '0.1000000000000000000001'
      }
    ]);
  });

  it('names native count fields without treating them as billed usage', () => {
    const response = parseNativeJSON(
      '{"input_tokens":9007199254740993,"cachedContentTokenCount":0}'
    );
    expect(nativeCountFields(response)).toEqual([
      { name: 'input_tokens', value: '9007199254740993' },
      { name: 'cachedContentTokenCount', value: '0' }
    ]);
  });
});
