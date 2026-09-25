import { describe, expect, it } from 'vitest';
import type { components } from '$lib/api/schema';
import { inspectionDialects } from './inspectionDialects';

type OperationDialect = components['schemas']['OperationDialect'];

const dialect = (
  id: string,
  operation: string,
  surface: string,
  mode: string
): OperationDialect => ({
  id,
  operation,
  surface,
  mode,
  model_binding: 'none',
  revision: 'v1',
  operation_revision: 'v1',
  label: id,
  documentation: '',
  evidence: ''
});

const catalog = [
  dialect('openai-chat', 'generation', 'openai', 'unary'),
  dialect('openai-responses', 'generation', 'openai', 'streaming'),
  dialect('anthropic-messages', 'generation', 'anthropic', 'unary'),
  dialect('voyage-embeddings', 'embeddings', 'native', 'unary'),
  dialect('voyage-embeddings', 'embeddings', 'native', 'unary'),
  dialect('tei-rerank', 'rerank', 'native', 'unary'),
  // Registered in the catalog but never offered: the simulation request
  // contract's closed dialect enum cannot send it.
  dialect('cohere-embed-v2', 'embeddings', 'native', 'unary')
];

describe('inspectionDialects', () => {
  it('filters catalog rows by operation, surface and transport mode', () => {
    expect(
      inspectionDialects(catalog, 'generation', 'openai', 'unary')
    ).toEqual(['openai-chat']);
    expect(
      inspectionDialects(catalog, 'generation', 'openai', 'streaming')
    ).toEqual(['openai-responses']);
    expect(inspectionDialects(catalog, 'generation', 'anthropic')).toEqual([
      'anthropic-messages'
    ]);
    // Duplicate registry rows collapse to one offered dialect.
    expect(inspectionDialects(catalog, 'embeddings', 'native')).toEqual([
      'voyage-embeddings'
    ]);
  });

  it('never offers registry rows the simulation contract cannot name', () => {
    expect(inspectionDialects(catalog, 'embeddings', 'native')).not.toContain(
      'cohere-embed-v2'
    );
    expect(inspectionDialects(catalog, 'embeddings', 'openai')).toEqual([]);
  });
});
