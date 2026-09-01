import { describe, expect, it } from 'vitest';
import { formatBudget, formatCost } from './format';

describe('exact budget formatting', () => {
  it('preserves the smallest supported positive budget', () => {
    expect(formatBudget('0.000000000001')).toBe('0.000000000001');
    expect(formatBudget('0.000000000123')).toBe('0.000000000123');
  });

  it('does not round across an integer boundary', () => {
    expect(formatBudget('999999999999.999999999999')).toBe(
      '999,999,999,999.999999999999'
    );
    expect(formatBudget('9999999999999999.999999999999')).toBe(
      '9,999,999,999,999,999.999999999999'
    );
  });

  it('formats currency without converting the amount to a Number', () => {
    expect(formatBudget('999999999999.999999999999', 'USD')).toBe(
      '$999,999,999,999.999999999999'
    );
    expect(formatBudget('0.000000000001', 'USD')).toBe('$0.000000000001');
  });

  it('preserves significant fractions and normalizes redundant zeros', () => {
    expect(formatBudget('00012.340000000000')).toBe('12.34');
    expect(formatBudget('0.015')).toBe('0.015');
    expect(formatBudget('10')).toBe('10.00');
    expect(formatBudget('0.000000000000')).toBe('0.0000');
  });

  it('does not represent missing or malformed values as zero', () => {
    expect(formatBudget(null)).toBe('—');
    expect(formatBudget(undefined)).toBe('—');
    expect(formatBudget('')).toBe('—');
    expect(formatBudget('invalid')).toBe('invalid');
    expect(formatBudget('invalid', 'USD')).toBe('invalid USD');
  });

  it('leaves the general cost summary formatter unchanged', () => {
    expect(formatCost('0.015')).toBe('0.02');
    expect(formatCost(null)).toBe('Unpriced');
  });
});
