import { describe, expect, it } from 'vitest';
import { policyError, policyForm, policyInput } from './policy';

describe('end-user policy forms', () => {
  it('preserves exact costs and empty overrides without mutating the saved policy', () => {
    const digest = 'a'.repeat(64);
    const original = {
      defaults: { daily_cost_limit: '0.000000000001', requests_per_minute: 3 },
      overrides: { [digest]: {} },
      blocked: [digest]
    };
    const form = policyForm(original);
    expect(policyError(form)).toBe('');
    const input = policyInput(form)!;
    expect(input.defaults?.daily_cost_limit).toBe('0.000000000001');
    expect(input.overrides?.[digest].requests_per_minute).toBeNull();
    form.defaults.requests_per_minute = '10';
    expect(original.defaults.requests_per_minute).toBe(3);
    form.enabled = false;
    expect(policyInput(form)).toBeNull();
  });

  it('rejects duplicate digests, raw identifiers, and unsafe numeric values', () => {
    const form = policyForm({});
    form.blocked = 'customer@example.com';
    expect(policyError(form)).toMatch(/digests/);
    form.blocked = `${'a'.repeat(64)}\n${'a'.repeat(64)}`;
    expect(policyError(form)).toMatch(/once/);
    form.blocked = '';
    for (const value of ['0', '-1', '1.1', '9007199254740992']) {
      form.defaults.tokens_per_minute = value;
      expect(policyError(form)).not.toBe('');
    }
    form.defaults.tokens_per_minute = '9007199254740991';
    expect(policyError(form)).toBe('');
  });
});
