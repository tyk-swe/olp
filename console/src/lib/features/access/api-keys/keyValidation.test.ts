import { describe, expect, it } from 'vitest';
import { validateApiKey } from './keyValidation';

describe('API key validation', () => {
  it('requires an intentional name and positive hard limits', () => {
    expect(validateApiKey({ name: ' ', requestsPerMinute: 0 })).toMatchObject({
      name: 'Enter a name.',
      requestsPerMinute: expect.any(String)
    });
  });

  it('accepts an unlimited key when limit fields are omitted', () => {
    expect(validateApiKey({ name: 'production SDK' })).toEqual({});
  });
});
