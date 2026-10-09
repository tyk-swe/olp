import { describe, expect, it } from 'vitest';
import { routeLimitForm, routeLimitsError, routeLimitsInput } from './policy';

describe('route limits', () => {
  it('preserves decimal precision and clears all route entries', () => {
    const policy = {
      chat: { daily_cost_limit: '0.000000000001', requests_per_minute: 12 }
    };
    const form = routeLimitForm(policy);
    expect(routeLimitsError(form)).toBe('');
    expect(routeLimitsInput(form).chat).toMatchObject(policy.chat);
    expect(routeLimitsInput([])).toEqual({});
    expect(policy.chat.daily_cost_limit).toBe('0.000000000001');
  });
  it('refuses duplicate routes and invalid limits', () => {
    const form = routeLimitForm({ chat: {} });
    expect(routeLimitsError([...form, ...form])).not.toBe('');
    form[0].limits.tokens_per_minute = '-1';
    expect(routeLimitsError(form)).not.toBe('');
  });
});
