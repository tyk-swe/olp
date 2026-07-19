import { describe, expect, it } from 'vitest';
import { parseResponseSchema, parseTools } from './validation';

describe('playground JSON fields', () => {
  it('accepts tools only as an array', () => {
    expect(parseTools('[{"name":"weather","input_schema":{}}]')).toHaveLength(1);
    expect(() => parseTools('{"type":"function"}')).toThrow('array');
  });

  it('wraps a strict structured-output schema', () => {
    expect(parseResponseSchema('{"type":"object"}')).toMatchObject({
      type: 'json_schema',
      name: 'playground_response',
      strict: true
    });
  });
});
