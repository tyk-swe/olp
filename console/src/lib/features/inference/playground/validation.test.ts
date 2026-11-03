import { describe, expect, it } from 'vitest';
import {
  parseMaxOutputTokens,
  parseResponseSchema,
  parseTemperature,
  parseTools
} from './validation';

describe('playground tool input', () => {
  it('omits blank input and keeps only the supported fields from valid tools', () => {
    expect(parseTools('  ')).toBeUndefined();
    expect(
      parseTools(
        JSON.stringify([
          {
            name: ' weather ',
            description: 'Look up conditions',
            input_schema: {
              type: 'object',
              properties: { city: { type: 'string' } }
            },
            extra: true
          },
          { name: 'time', input_schema: { type: 'object' } }
        ])
      )
    ).toEqual([
      {
        name: 'weather',
        description: 'Look up conditions',
        input_schema: {
          type: 'object',
          properties: { city: { type: 'string' } }
        }
      },
      { name: 'time', input_schema: { type: 'object' } }
    ]);
  });

  it.each([
    '{}',
    '[null]',
    '[{}]',
    '[{"name":" ","input_schema":{}}]',
    '[{"name":"tool"}]',
    '[{"name":"tool","description":3,"input_schema":{}}]'
  ])('rejects an invalid tools value %s', (value) => {
    expect(() => parseTools(value)).toThrow(
      'Tools must be an array of name, description, and input_schema objects.'
    );
  });

  it('explains malformed JSON', () => {
    expect(() => parseTools('[invalid')).toThrow('Enter valid JSON.');
  });
});

describe('playground structured responses', () => {
  it('omits blank input or wraps a valid schema for a strict response', () => {
    const schema = {
      type: 'object',
      properties: { answer: { type: 'string' } }
    };

    expect(parseResponseSchema('  ')).toBeUndefined();
    expect(parseResponseSchema(JSON.stringify(schema))).toEqual({
      type: 'json_schema',
      name: 'playground_response',
      strict: true,
      schema
    });
  });

  it.each(['null', '[]', 'true', '42', '"object"'])(
    'rejects a schema that is not an object: %s',
    (value) => {
      expect(() => parseResponseSchema(value)).toThrow(
        'The response schema must be a JSON object.'
      );
    }
  );

  it('explains malformed schema JSON', () => {
    expect(() => parseResponseSchema('{invalid')).toThrow('Enter valid JSON.');
  });
});

describe('playground generation limits', () => {
  it.each([
    ['0', 0],
    [' 1.25 ', 1.25],
    ['2', 2]
  ])('accepts a temperature of %s', (value, expected) => {
    expect(parseTemperature(value)).toBe(expected);
  });

  it.each(['-0.01', '2.01', 'NaN', 'Infinity'])(
    'rejects an invalid temperature %s',
    (value) => {
      expect(() => parseTemperature(value)).toThrow(
        'Temperature must be from 0 through 2.'
      );
    }
  );

  it.each([
    ['1', 1],
    [' 42 ', 42],
    ['1000000', 1000000]
  ])('accepts an output token limit of %s', (value, expected) => {
    expect(parseMaxOutputTokens(value)).toBe(expected);
  });

  it.each(['0', '-1', '1.5', '1000001', 'Infinity', 'invalid'])(
    'rejects an invalid output token limit %s',
    (value) => {
      expect(() => parseMaxOutputTokens(value)).toThrow(
        'Maximum output tokens must be from 1 through 1000000.'
      );
    }
  );

  it('omits blank optional limits', () => {
    expect(parseTemperature('  ')).toBeUndefined();
    expect(parseMaxOutputTokens('  ')).toBeUndefined();
  });
});
