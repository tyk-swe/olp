import { describe, expect, it } from 'vitest';
import {
  parseMaxOutputTokens,
  parseResponseSchema,
  parseTemperature,
  parseTools
} from './validation';

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

  it('handles response schema error paths and empty inputs', () => {
    expect(parseResponseSchema('')).toBeUndefined();
    expect(parseResponseSchema('   ')).toBeUndefined();

    expect(() => parseResponseSchema('[]')).toThrow(
      'The response schema must be a JSON object.'
    );
    expect(() => parseResponseSchema('null')).toThrow(
      'The response schema must be a JSON object.'
    );
    expect(() => parseResponseSchema('"string"')).toThrow(
      'The response schema must be a JSON object.'
    );
    expect(() => parseResponseSchema('123')).toThrow(
      'The response schema must be a JSON object.'
    );
    expect(() => parseResponseSchema('true')).toThrow(
      'The response schema must be a JSON object.'
    );

    expect(() => parseResponseSchema('{invalid}')).toThrow('Enter valid JSON.');
  });
});

describe('playground sampling fields', () => {
  it('leaves an empty control to the provider default', () => {
    expect(parseTemperature('')).toBeUndefined();
    expect(parseTemperature('  ')).toBeUndefined();
    expect(parseMaxOutputTokens('')).toBeUndefined();
  });

  it('accepts the inclusive temperature bounds', () => {
    expect(parseTemperature('0')).toBe(0);
    expect(parseTemperature('0.7')).toBe(0.7);
    expect(parseTemperature('2')).toBe(2);
  });

  it('rejects a temperature the backend would reject', () => {
    expect(() => parseTemperature('-0.1')).toThrow('Temperature must be from 0 through 2.');
    expect(() => parseTemperature('2.1')).toThrow('Temperature must be from 0 through 2.');
    expect(() => parseTemperature('warm')).toThrow('Temperature must be from 0 through 2.');
  });

  it('accepts the inclusive output-token bounds', () => {
    expect(parseMaxOutputTokens('1')).toBe(1);
    expect(parseMaxOutputTokens(' 1000000 ')).toBe(1_000_000);
  });

  it('rejects zero, fractional, and oversized output-token limits', () => {
    const message = 'Maximum output tokens must be from 1 through 1000000.';
    expect(() => parseMaxOutputTokens('0')).toThrow(message);
    expect(() => parseMaxOutputTokens('1.5')).toThrow(message);
    expect(() => parseMaxOutputTokens('1000001')).toThrow(message);
    expect(() => parseMaxOutputTokens('lots')).toThrow(message);
  });
});
