import { describe, expect, it } from 'vitest';
import {
  dateTimeLocalValue,
  formatBudget,
  formatBytes,
  formatCompact,
  formatCost,
  formatDate,
  formatDay,
  formatInteger,
  formatSignedPercent,
  formatTime,
  stateLabel,
  statusLabel,
  statusTone
} from './format';

describe('date and time presentation', () => {
  it('uses a placeholder for missing or invalid timestamps', () => {
    expect(formatDate(null)).toBe('—');
    expect(formatDate('invalid')).toBe('—');
    expect(formatDay('')).toBe('—');
    expect(formatDay('invalid')).toBe('—');
    expect(formatTime(undefined)).toBe('—');
    expect(formatTime('invalid')).toBe('—');
    expect(formatTime(0)).not.toBe('—');
  });

  it('labels the same UTC day consistently across offset representations', () => {
    expect(formatDay('2025-02-10T01:02:00Z')).toBe(
      formatDay('2025-02-09T20:02:00-05:00')
    );
  });

  it('round trips a timestamp through a local date and time input', () => {
    const instant = new Date('2025-02-10T12:34:00Z');
    const local = dateTimeLocalValue(instant);

    expect(local).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/);
    expect(new Date(local).valueOf()).toBe(instant.valueOf());
    expect(dateTimeLocalValue(instant.toISOString())).toBe(local);
    expect(dateTimeLocalValue('invalid')).toBe('');
  });
});

describe('number and amount presentation', () => {
  it('distinguishes missing, zero and invalid counts', () => {
    expect(formatCompact(null)).toBe('—');
    expect(formatCompact(0)).toBe('0');
    expect(formatCompact('invalid')).toBe('invalid');
    expect(formatCompact('1234')).toBe(formatCompact(1234));
    expect(formatInteger('')).toBe('—');
    expect(formatInteger(0)).toBe('0');
    expect(formatInteger('invalid')).toBe('invalid');
    expect(formatInteger('1234')).toBe(formatInteger(1234));
    expect(formatInteger(12.6)).toBe('13');
  });

  it('shows signed percentage changes and treats rounded zero as unsigned', () => {
    expect(formatSignedPercent(0.032)).toBe('+3.2%');
    expect(formatSignedPercent(-0.004)).toBe('-0.4%');
    expect(formatSignedPercent(-0.00001)).toBe('0.0%');
    expect(formatSignedPercent(null)).toBe('—');
    expect(formatSignedPercent(Number.POSITIVE_INFINITY)).toBe('—');
  });

  it('keeps small, missing and unparseable prices distinct', () => {
    expect(formatCost(null)).toBe('Unpriced');
    expect(formatCost('0')).toBe('0.0000');
    expect(formatCost('0.009')).toBe('0.0090');
    expect(formatCost('1.50')).toBe('1.50');
    expect(formatCost('0.0000001')).toBe('<0.000001');
    expect(formatCost('invalid')).toBe('invalid');
    expect(formatCost('invalid', 'USD')).toBe('invalid USD');
  });

  it('preserves exact budget digits beyond floating point precision', () => {
    const amount = '9007199254740993.123400';

    expect(formatBudget(amount)).toBe('9,007,199,254,740,993.1234');
    expect(formatCost(amount)).toBe(formatBudget(amount));
    expect(formatBudget('00042.1000')).toBe('42.10');
    expect(formatBudget('0.000012300')).toBe('0.0000123');
    expect(formatBudget('not-a-number', 'USD')).toBe('not-a-number USD');
    expect(formatBudget()).toBe('—');
  });
});

describe('byte and status presentation', () => {
  it.each([
    [0, '0 B'],
    [512, '512 B'],
    [1024, '1.00 KiB'],
    [10 * 1024, '10.0 KiB'],
    [100 * 1024, '100 KiB'],
    [-1.5 * 1024 * 1024, '-1.50 MiB']
  ])('formats %s bytes as %s', (value, formatted) => {
    expect(formatBytes(value)).toBe(formatted);
    expect(formatBytes(String(value))).toBe(formatted);
  });

  it('uses a placeholder when byte counts are missing or invalid', () => {
    expect(formatBytes()).toBe('—');
    expect(formatBytes('invalid')).toBe('—');
    expect(formatBytes(Number.POSITIVE_INFINITY)).toBe('—');
  });

  it.each([
    [199, ''],
    [200, 'success'],
    [399, 'success'],
    [400, 'warning'],
    [499, 'warning'],
    [500, 'danger']
  ])('assigns the status tone for %s', (status, tone) => {
    expect(statusTone(status)).toBe(tone);
  });

  it('gives errors precedence over status and labels absent states', () => {
    expect(statusTone(200, 'request_failed')).toBe('danger');
    expect(statusTone(null)).toBe('');
    expect(statusLabel(200, 'request_failed')).toBe('request_failed');
    expect(statusLabel(null)).toBe('In progress');
    expect(statusLabel(200)).toBe('200');
    expect(stateLabel('not_ready')).toBe('not ready');
    expect(stateLabel()).toBe('unknown');
  });
});
