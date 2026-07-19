import { describe, expect, it } from 'vitest';
import { dateTimeLocalValue, formatCompact, formatCost, statusLabel, statusTone } from './format';

describe('operations formatting', () => {
  it('never represents missing pricing as zero', () => {
    expect(formatCost(null)).toBe('Unpriced');
    expect(formatCost('0')).toContain('0');
  });

  it('keeps error classes more informative than status codes', () => {
    expect(statusLabel(503, 'upstream_timeout')).toBe('upstream_timeout');
    expect(statusTone(429)).toBe('warning');
    expect(statusTone(200)).toBe('success');
  });

  it('compacts large token totals', () => {
    expect(formatCompact('12000')).toMatch(/12K|12k/);
  });

  it('formats UTC instants as local wall time for datetime-local controls', () => {
    const instant = new Date('2026-07-13T12:34:00Z');
    Object.defineProperty(instant, 'getTimezoneOffset', { value: () => 240 });

    expect(dateTimeLocalValue(instant)).toBe('2026-07-13T08:34');
  });
});
