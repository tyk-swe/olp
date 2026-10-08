import { describe, expect, it } from 'vitest';
import { budgetError, budgetForm, budgetInput } from './policy';

describe('attribution budgets', () => {
  it('preserves exact costs and independent pairs without mutating saved data', () => {
    const source = {
      team: {
        core: { weekly_cost_limit: '0.000000000001' },
        edge: { daily_cost_limit: '2' }
      }
    };
    const form = budgetForm(source);
    expect(budgetError(form)).toBe('');
    expect(budgetInput(form).team.core.weekly_cost_limit).toBe(
      '0.000000000001'
    );
    form[0].limits.weekly_cost_limit = '4';
    expect(source.team.core.weekly_cost_limit).toBe('0.000000000001');
    expect(budgetInput([])).toEqual({});
  });
  it('rejects ambiguous pairs, unbounded entries and invalid values', () => {
    const form = budgetForm({ team: { core: { daily_cost_limit: '2' } } });
    expect(budgetError([...form, { ...form[0], id: 'duplicate' }])).not.toBe(
      ''
    );
    form[0].limits.daily_cost_limit = '';
    expect(budgetError(form)).not.toBe('');
    form[0].limits.daily_cost_limit = '2';
    form[0].value = 'email@example.com';
    expect(budgetError(form)).not.toBe('');
  });
});
