import { describe, expect, it } from 'vitest';
import { isLimitsOutagePolicy, optionalDecimal } from './validation';

describe('installation settings validation', () => {
  it('accepts exact decimal text and preserves missing prices', () => {
    expect(optionalDecimal('0.000125')).toBe('0.000125');
    expect(optionalDecimal('')).toBeNull();
    expect(() => optionalDecimal('-1')).toThrow('non-negative');
  });

  it('accepts only the two limits outage policies', () => {
    expect(isLimitsOutagePolicy('fail_closed')).toBe(true);
    expect(isLimitsOutagePolicy('fail_open')).toBe(true);
    expect(isLimitsOutagePolicy('open')).toBe(false);
    expect(isLimitsOutagePolicy('')).toBe(false);
  });
});
