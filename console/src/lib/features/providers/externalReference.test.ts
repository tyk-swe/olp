import { describe, expect, it } from 'vitest';
import { parseExternalReference } from './externalReference';

describe('external credential references', () => {
  it('retains explicit store and version without accepting arbitrary fields', () => {
    const value = {
      store: 'gcp',
      secret_id: 'projects/project/secrets/provider',
      version: '3'
    };
    expect(parseExternalReference(JSON.stringify(value))).toEqual(value);
    for (const bad of [
      'null',
      '[]',
      '{}',
      '{"store":"other","secret_id":"provider","version":"3"}',
      JSON.stringify({ ...value, credential: 'private-value' })
    ])
      expect(() => parseExternalReference(bad)).toThrow();
  });
});
