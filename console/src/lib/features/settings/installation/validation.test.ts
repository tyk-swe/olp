import { describe, expect, it } from 'vitest';
import { optionalDecimal } from './validation';

describe('installation settings validation', () => {
  it('accepts exact decimal text and preserves missing prices', () => {
    expect(optionalDecimal('0.000125')).toBe('0.000125');
    expect(optionalDecimal('')).toBeNull();
    expect(() => optionalDecimal('-1')).toThrow('non-negative');
  });
});
