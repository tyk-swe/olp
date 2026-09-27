import { expect, it } from 'vitest';
import { optionalDecimal } from './pricingValidation';

it('keeps exact decimal strings and missing prices distinct from zero', () => {
  expect(optionalDecimal(' 0.00012500 ')).toBe('0.00012500');
  expect(optionalDecimal('9007199254740993.000000001')).toBe(
    '9007199254740993.000000001'
  );
  expect(optionalDecimal('0')).toBe('0');
  expect(optionalDecimal('')).toBeNull();
  expect(optionalDecimal(' ')).toBeNull();
});

it.each(['-1', '1e2', 'NaN', 'Infinity', '1,000', '.5', '1.'])(
  'rejects invalid decimal %s',
  (value) => {
    expect(() => optionalDecimal(value)).toThrow('non-negative decimal');
  }
);
