import { describe, expect, it } from 'vitest';
import { timeValid } from '$lib/lists/filters';

describe('timeValid', () => {
  it('validates calendar days without JavaScript date normalization', () => {
    expect(timeValid('2024-02-29T12:00:00Z', true)).toBe(true);
    expect(timeValid('2026-02-29T12:00:00Z', true)).toBe(false);
    expect(timeValid('2026-02-30T12:00:00Z', true)).toBe(false);
    expect(timeValid('2026-07-12T24:00:00Z', true)).toBe(false);
  });

  it('requires a timezone only for URL values', () => {
    expect(timeValid('2026-07-12T12:00', false)).toBe(true);
    expect(timeValid('2026-07-12T12:00', true)).toBe(false);
    expect(timeValid('2026-07-12T12:00:00+02:00', true)).toBe(true);
  });
});
