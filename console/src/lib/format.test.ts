import { describe, expect, it } from 'vitest';
import {
  dateTimeLocalValue,
  formatBytes,
  formatCompact,
  formatCost,
  formatDay,
  formatInteger,
  statusLabel,
  statusTone
} from './format';

describe('shared formatting', () => {
  it('never represents missing pricing as zero', () => {
    expect(formatCost(null)).toBe('Unpriced');
    expect(formatCost('0')).toContain('0');
  });

  it('omits the currency symbol when the record has no currency', () => {
    const withCurrency = formatCost('12.5', 'USD');
    const withoutCurrency = formatCost('12.5', null);

    expect(withCurrency).toMatch(/\$|USD/);
    expect(withoutCurrency).not.toMatch(/\$|USD/);
    expect(withoutCurrency).toContain('12.5');
    expect(formatCost('12.5')).toBe(withoutCurrency);
  });

  it('groups full token counts without compacting them', () => {
    expect(formatInteger(1234567)).toMatch(/1.234.567/);
    expect(formatInteger(null)).toBe('—');
    expect(formatInteger(undefined)).toBe('—');
    expect(formatInteger('4096')).toContain('4');
  });

  it('keeps error classes more informative than status codes', () => {
    expect(statusLabel(503, 'upstream_timeout')).toBe('upstream_timeout');
    expect(statusTone(429)).toBe('warning');
    expect(statusTone(400)).toBe('warning');
    expect(statusTone(500)).toBe('danger');
    expect(statusTone(200)).toBe('success');
  });

  it('compacts large token totals', () => {
    expect(formatCompact('12000')).toMatch(/12K|12k/);
  });

  it('drops the time of day from daily bucket labels', () => {
    const label = formatDay('2026-07-13T00:00:00Z');

    expect(label).not.toMatch(/\d{1,2}:\d{2}/);
    expect(label).toMatch(/2026/);
    expect(formatDay(null)).toBe('—');
    expect(formatDay('not-a-date')).toBe('—');
  });

  it('scales byte counts to binary units', () => {
    expect(formatBytes(512)).toBe('512 B');
    expect(formatBytes(1024)).toBe('1.00 KiB');
    expect(formatBytes(1_572_864)).toBe('1.50 MiB');
    expect(formatBytes(1_073_741_824)).toBe('1.00 GiB');
    expect(formatBytes(0)).toBe('0 B');
  });

  it('reports an unmeasured byte count instead of zero', () => {
    expect(formatBytes(null)).toBe('—');
    expect(formatBytes(undefined)).toBe('—');
    expect(formatBytes('not-a-number')).toBe('—');
  });

  it('formats UTC instants as local wall time for datetime-local controls', () => {
    const instant = new Date('2026-07-13T12:34:00Z');
    Object.defineProperty(instant, 'getTimezoneOffset', { value: () => 240 });

    expect(dateTimeLocalValue(instant)).toBe('2026-07-13T08:34');
  });
});
