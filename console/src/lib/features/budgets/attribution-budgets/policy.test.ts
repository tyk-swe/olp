import { describe, expect, it } from 'vitest';
import { budgetError, budgetForm, budgetInput } from './policy';

const configured = {
  team: {
    alpha: {
      daily_cost_limit: '1.000000000001',
      cost_basis: 'charge' as const
    },
    beta: { monthly_cost_limit: '200.50', cost_basis: 'cost' as const }
  }
};

describe('attribution budgets', () => {
  it('preserves each budget basis and exact amount when another row is edited', () => {
    const rows = budgetForm(configured);
    const changed = rows.map((row) =>
      row.value === 'beta'
        ? { ...row, limits: { ...row.limits, monthly_cost_limit: '250.75' } }
        : row
    );

    expect(budgetError(changed)).toBe('');
    expect(budgetInput(changed)).toEqual({
      team: {
        alpha: {
          daily_cost_limit: '1.000000000001',
          weekly_cost_limit: null,
          monthly_cost_limit: null,
          cost_basis: 'charge'
        },
        beta: {
          daily_cost_limit: null,
          weekly_cost_limit: null,
          monthly_cost_limit: '250.75',
          cost_basis: 'cost'
        }
      }
    });
  });

  it('trims labels, values and caps while grouping budgets', () => {
    const rows = budgetForm(configured).map((row) => ({
      ...row,
      label: ` ${row.label} `,
      value: ` ${row.value} `,
      limits: { ...row.limits, weekly_cost_limit: ' 3.25 ' }
    }));

    expect(budgetError(rows)).toBe('');
    expect(budgetInput(rows)).toMatchObject({
      team: {
        alpha: { weekly_cost_limit: '3.25' },
        beta: { weekly_cost_limit: '3.25' }
      }
    });
  });

  it('requires unique valid label and value pairs', () => {
    const [first] = budgetForm(configured);
    expect(
      budgetError([first, { ...first, label: ` ${first.label} ` }])
    ).toContain('valid, unique');
    expect(budgetError([{ ...first, label: 'invalid label' }])).toContain(
      'valid, unique'
    );
    expect(budgetError([{ ...first, value: '' }])).toContain('valid, unique');
  });

  it('requires a positive cost cap within supported precision', () => {
    const [first] = budgetForm(configured);
    const noCap = {
      ...first,
      limits: { ...first.limits, daily_cost_limit: '' }
    };
    expect(budgetError([noCap])).toContain('at least one cost cap');
    expect(
      budgetError([
        { ...noCap, limits: { ...noCap.limits, daily_cost_limit: '0' } }
      ])
    ).toContain('positive decimals');
    expect(
      budgetError([
        {
          ...noCap,
          limits: { ...noCap.limits, daily_cost_limit: '0.0000000000001' }
        }
      ])
    ).toContain('positive decimals');
  });

  it('enforces the maximum number of label and value budgets', () => {
    const [first] = budgetForm(configured);
    const rows = Array.from({ length: 65 }, (_, index) => ({
      ...first,
      value: `value-${index}`
    }));
    expect(budgetError(rows)).toContain('at most 64');
    expect(budgetError(rows.slice(0, 64))).toBe('');
  });
});
