import { describe, expect, it } from 'vitest';
import {
  budgetState,
  budgetStateLabel,
  budgetStateNote,
  budgetWindowState,
  compareDecimalStrings,
  currentBudgetLimit,
  type ApiKeyBudget
} from './budgetPresentation';

const unlimited: ApiKeyBudget = {
  daily: { accrued: '0', limit: null, window_ends_at: '2025-01-02T00:00:00Z' },
  monthly: {
    accrued: '0',
    limit: null,
    window_ends_at: '2025-02-01T00:00:00Z'
  },
  unpriced_attempts: 0
};

describe('API key budget presentation', () => {
  it.each([
    ['0', '0.00', 0],
    ['00042.10', '42.1', 0],
    ['9', '10', -1],
    ['10', '9', 1],
    ['0.009', '0.01', -1],
    ['0.01001', '0.01', 1],
    ['9007199254740993.0000000001', '9007199254740993.0000000002', -1],
    ['99999999999999999', '100000000000000000', -1]
  ])('compares exact decimal amounts %s and %s', (left, right, order) => {
    expect(compareDecimalStrings(left, right)).toBe(order);
    expect(compareDecimalStrings(right, left)).toBe(order === 0 ? 0 : -order);
  });

  it.each(['', ' 1', '1 ', '-1', '1e3', '1.', '.1', 'NaN'])(
    'does not claim an order for invalid amount %s',
    (value) => {
      expect(compareDecimalStrings(value, '1')).toBeNull();
      expect(compareDecimalStrings('1', value)).toBeNull();
    }
  );

  it('uses the effective limit when available, including an explicit unlimited limit', () => {
    expect(currentBudgetLimit(undefined)).toBeNull();
    expect(currentBudgetLimit({ limit: '10' })).toBe('10');
    expect(currentBudgetLimit({ limit: '10', effective_limit: '15' })).toBe(
      '15'
    );
    expect(
      currentBudgetLimit({ limit: '10', effective_limit: null })
    ).toBeNull();
  });

  it('distinguishes disabled, unlimited, unknown, within and exhausted windows', () => {
    const daily = unlimited.daily;
    expect(
      budgetWindowState({ ...unlimited, enforcement_active: false }, 'daily')
    ).toBe('policy');
    expect(budgetWindowState(unlimited, 'weekly')).toBe('unknown');
    expect(budgetWindowState(unlimited, 'daily')).toBe('unlimited');
    expect(
      budgetWindowState(
        { ...unlimited, daily: { ...daily, limit: '2', accrued: '1.999' } },
        'daily'
      )
    ).toBe('within');
    expect(
      budgetWindowState(
        { ...unlimited, daily: { ...daily, limit: '2', accrued: '2.000' } },
        'daily'
      )
    ).toBe('exhausted');
    expect(
      budgetWindowState(
        { ...unlimited, daily: { ...daily, limit: '2', accrued: 'unknown' } },
        'daily'
      )
    ).toBe('unknown');
    expect(
      budgetWindowState(
        {
          ...unlimited,
          daily: { ...daily, limit: '2', accrued: '2', effective_limit: '3' }
        },
        'daily'
      )
    ).toBe('within');
  });

  it('reports the most restrictive applicable budget window', () => {
    const window = unlimited.daily;
    const within = { ...window, limit: '5', accrued: '4' };
    const exhausted = { ...window, limit: '5', accrued: '5' };
    const unknown = { ...window, limit: '5', accrued: 'invalid' };

    expect(budgetState(unlimited)).toBe('unlimited');
    expect(budgetState({ ...unlimited, daily: within })).toBe('within');
    expect(budgetState({ ...unlimited, daily: within, monthly: unknown })).toBe(
      'unknown'
    );
    expect(
      budgetState({ ...unlimited, daily: unknown, weekly: exhausted })
    ).toBe('exhausted');
    expect(
      budgetState({ ...unlimited, daily: exhausted, enforcement_active: false })
    ).toBe('policy');
  });

  it('explains unusual budget states without claiming unused headroom', () => {
    expect(budgetStateLabel('unlimited')).toBe('No cost budget');
    expect(budgetStateLabel('within')).toBe('Within budget');
    expect(budgetStateLabel('exhausted')).toBe('Budget exhausted');
    expect(budgetStateLabel('unknown')).toBe('Budget state unknown');
    expect(budgetStateLabel('policy')).toContain('not enforced');
    expect(budgetStateNote('exhausted')).toContain('refused');
    expect(budgetStateNote('unknown')).toContain('unknown');
    expect(budgetStateNote('policy')).toContain('inactive');
    expect(budgetStateNote('within')).toBeNull();
    expect(budgetStateNote('unlimited')).toBeNull();
  });
});
