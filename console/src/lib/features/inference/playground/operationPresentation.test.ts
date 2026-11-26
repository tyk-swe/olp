import { describe, expect, it } from 'vitest';
import { parseNativeJSON } from '#lib/nativeJson.js';
import {
  nativeCountFields,
  rerankRows,
  vectorRows
} from './operationPresentation';

function expectedVector(
  overrides: Partial<{
    position: number;
    inputIndex: string;
    layout: string;
    dtype: string;
    logicalShape: string;
    storageShape: string;
  }>
) {
  return {
    position: 0,
    inputIndex: 'Not reported',
    layout: 'Dense array',
    dtype: 'Not declared',
    logicalShape: 'Unknown',
    storageShape: 'Unknown',
    ...overrides
  };
}

describe('embedding result presentation', () => {
  it.each([
    {
      name: 'dense values',
      response: { data: [{ index: 3, embedding: [0.1, 0.2] }] },
      request: { output_dtype: 'float', dimensions: 2 },
      row: expectedVector({
        inputIndex: '3',
        dtype: 'float',
        logicalShape: '2',
        storageShape: '2 stored values'
      })
    },
    {
      name: 'empty dense vector',
      response: { data: [{ embedding: [] }] },
      request: undefined,
      row: expectedVector({
        logicalShape: '0',
        storageShape: '0 stored values'
      })
    },
    {
      name: 'sparse coordinates',
      response: { predictions: [{ values: [{ index: 4, value: 0.5 }] }] },
      request: { dimensions: 12 },
      row: expectedVector({
        layout: 'Sparse coordinates',
        logicalShape: '12',
        storageShape: '1 index/value pairs'
      })
    },
    {
      name: 'token vectors',
      response: {
        embeddings: [
          [
            [1, 2],
            [3, 4]
          ]
        ]
      },
      request: {},
      row: expectedVector({
        layout: 'Multivector',
        logicalShape: '2 × 2',
        storageShape: '2 token vectors · 4 stored values'
      })
    },
    {
      name: 'packed bytes',
      response: { data: [{ embedding: [1, 2] }] },
      request: { output_dtype: 'ubinary' },
      row: expectedVector({
        layout: 'Packed binary',
        dtype: 'ubinary',
        logicalShape: '16',
        storageShape: '2 stored bytes'
      })
    },
    {
      name: 'encoded float storage',
      response: ['AQIDBA=='],
      request: { output_dtype: 'float' },
      row: expectedVector({
        layout: 'Base64 storage',
        dtype: 'float',
        logicalShape: '1',
        storageShape: '4 stored bytes'
      })
    },
    {
      name: 'invalid encoded storage',
      response: ['not base64'],
      request: { output_dtype: 'float' },
      row: expectedVector({
        layout: 'Base64 storage',
        dtype: 'float',
        storageShape: 'Opaque encoded value'
      })
    }
  ])(
    'describes $name without changing the native result',
    ({ response, request, row }) => {
      expect(vectorRows(response, request)).toEqual([row]);
    }
  );

  it('describes each typed embedding separately and uses the requested dimension', () => {
    const response = {
      embeddings: {
        float: [[0.1, 0.2]],
        ubinary: [[255]],
        base64: ['AQI=']
      }
    };

    expect(vectorRows(response, { output_dimension: 7 })).toEqual([
      expectedVector({
        position: 0,
        inputIndex: '0',
        dtype: 'float',
        logicalShape: '7',
        storageShape: '2 stored values'
      }),
      expectedVector({
        position: 1,
        inputIndex: '0',
        layout: 'Packed binary',
        dtype: 'ubinary',
        logicalShape: '7',
        storageShape: '1 stored byte'
      }),
      expectedVector({
        position: 2,
        inputIndex: '0',
        layout: 'Base64 storage',
        dtype: 'base64',
        logicalShape: '7',
        storageShape: '2 stored bytes'
      })
    ]);
  });

  it('does not report an empty dense vector as sparse coordinates', () => {
    expect(vectorRows({ data: [{ embedding: [] }] })).toEqual([
      {
        position: 0,
        inputIndex: 'Not reported',
        layout: 'Dense array',
        dtype: 'Not declared',
        logicalShape: '0',
        storageShape: '0 stored values'
      }
    ]);
  });

  it('bounds the number of rows shown for large responses', () => {
    const response = Array.from({ length: 140 }, (_, index) => ({
      index,
      embedding: [index]
    }));

    const rows = vectorRows(response);
    expect(rows).toHaveLength(128);
    expect(rows.at(-1)?.inputIndex).toBe('127');

    const typedRows = vectorRows({
      embeddings: { float: Array.from({ length: 140 }, (_, index) => [index]) }
    });
    expect(typedRows).toHaveLength(128);
    expect(typedRows.at(-1)?.inputIndex).toBe('127');
  });
});

describe('rerank and token count presentation', () => {
  it('keeps result order, maps document ids and preserves native scores', () => {
    const response = parseNativeJSON(
      '{"results":[{"index":1,"relevance_score":0.1234567890123456789},{"index":0,"score":0.1234567890123456789}]}'
    );
    const request = { documents: [{ id: 'first' }, { id: 'second' }] };

    expect(rerankRows(response, request)).toEqual([
      {
        position: 0,
        inputIndex: '1',
        inputId: '"second"',
        score: '0.1234567890123456789'
      },
      {
        position: 1,
        inputIndex: '0',
        inputId: '"first"',
        score: '0.1234567890123456789'
      }
    ]);
  });

  it('shows returned ids and unknown values when request documents cannot be matched', () => {
    expect(
      rerankRows({ data: [{ index: 3, id: 'result' }, {}] }, { documents: [] })
    ).toEqual([
      { position: 0, inputIndex: '3', inputId: '"result"', score: 'Unknown' },
      {
        position: 1,
        inputIndex: 'Unknown',
        inputId: 'Not reported',
        score: 'Unknown'
      }
    ]);
  });

  it('shows recognized native token counts without rounding', () => {
    const response = parseNativeJSON(
      '{"input_tokens":9007199254740993,"totalTokens":9007199254740994,"other":5}'
    );

    expect(nativeCountFields(response)).toEqual([
      { name: 'input_tokens', value: '9007199254740993' },
      { name: 'totalTokens', value: '9007199254740994' }
    ]);
    expect(nativeCountFields(null)).toEqual([]);
  });
});
