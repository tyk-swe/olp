import { describe, expect, it } from 'vitest';
import {
  budgetState,
  budgetStateLabel,
  budgetStateNote,
  budgetWindowState,
  compareDecimalStrings,
  type ApiKeyBudget
} from '$lib/features/access/api-keys/budgetPresentation';

function budget(overrides: Partial<ApiKeyBudget> = {}): ApiKeyBudget {
  return {
    enforcement_active: true,
    daily: {
      limit: '10.00',
      accrued: '2.500000',
      window_ends_at: '2026-07-13T00:00:00Z'
    },
    monthly: {
      limit: '100.00',
      accrued: '12.25',
      window_ends_at: '2026-08-01T00:00:00Z'
    },
    unpriced_attempts: 0,
    ...overrides
  };
}

describe('compareDecimalStrings', () => {
  it('orders by magnitude rather than by digit count', () => {
    expect(compareDecimalStrings('9', '10')).toBe(-1);
    expect(compareDecimalStrings('10', '9')).toBe(1);
    expect(compareDecimalStrings('0009', '9')).toBe(0);
  });

  it('pads fractions before comparing them', () => {
    expect(compareDecimalStrings('1.5', '1.50')).toBe(0);
    expect(compareDecimalStrings('1.5', '1.05')).toBe(1);
    expect(compareDecimalStrings('1.05', '1.5')).toBe(-1);
    expect(compareDecimalStrings('2', '2.000001')).toBe(-1);
  });

  it('keeps precision a double would lose', () => {
    const accrued = '10.000000000000000001';
    expect(Number(accrued) < Number('10')).toBe(false);
    expect(compareDecimalStrings(accrued, '10')).toBe(1);
    expect(compareDecimalStrings('0.1', '0.10000000000000000001')).toBe(-1);
    expect(
      compareDecimalStrings(
        '123456789012345678901234567890.5',
        '123456789012345678901234567890.50'
      )
    ).toBe(0);
  });

  it('refuses anything that is not a plain non-negative decimal', () => {
    for (const value of [
      '',
      ' 1',
      '1 ',
      '1\n',
      '1\r',
      '1\r\n',
      '1e3',
      '-1',
      '+1',
      '1.',
      '.5',
      '1,5',
      'NaN',
      'Infinity',
      '0x10'
    ])
      expect(compareDecimalStrings(value, '1')).toBeNull();
    expect(compareDecimalStrings('1', 'unpriced')).toBeNull();
  });
});

describe('budgetWindowState', () => {
  it('reports headroom below the limit', () => {
    expect(budgetWindowState(budget(), 'daily')).toBe('within');
    expect(budgetWindowState(budget(), 'monthly')).toBe('within');
  });

  it('treats reaching the limit as exhausted, because the gateway does', () => {
    const reached = budget({
      daily: {
        limit: '10.00',
        accrued: '10',
        window_ends_at: '2026-07-13T00:00:00Z'
      }
    });
    expect(budgetWindowState(reached, 'daily')).toBe('exhausted');
    const past = budget({
      monthly: {
        limit: '100.00',
        accrued: '100.000000000000001',
        window_ends_at: '2026-08-01T00:00:00Z'
      }
    });
    expect(budgetWindowState(past, 'monthly')).toBe('exhausted');
  });

  it('reports an absent limit as unlimited', () => {
    const open = budget({
      daily: {
        limit: null,
        accrued: '41.99',
        window_ends_at: '2026-07-13T00:00:00Z'
      }
    });
    expect(budgetWindowState(open, 'daily')).toBe('unlimited');
  });

  it('never claims headroom for amounts it cannot order', () => {
    const malformed = budget({
      daily: {
        limit: '10.00',
        accrued: '1e1',
        window_ends_at: '2026-07-13T00:00:00Z'
      }
    });
    expect(budgetWindowState(malformed, 'daily')).toBe('unknown');
  });

  it('reports stored policy when accounting is not live', () => {
    const saved = budget({ enforcement_active: false });
    expect(budgetWindowState(saved, 'daily')).toBe('policy');
    expect(budgetWindowState(saved, 'monthly')).toBe('policy');
  });

  it('treats a missing enforcement flag as live accounting', () => {
    const absent = budget();
    delete absent.enforcement_active;
    expect(budgetWindowState(absent, 'daily')).toBe('within');
  });
});

describe('budgetState', () => {
  it('takes the most restrictive window', () => {
    expect(budgetState(budget())).toBe('within');
    expect(
      budgetState(
        budget({
          monthly: {
            limit: '100.00',
            accrued: '100.00',
            window_ends_at: '2026-08-01T00:00:00Z'
          }
        })
      )
    ).toBe('exhausted');
    expect(
      budgetState(
        budget({
          daily: {
            limit: 'not-a-decimal',
            accrued: '0',
            window_ends_at: '2026-07-13T00:00:00Z'
          }
        })
      )
    ).toBe('unknown');
  });

  it('prefers exhaustion over an unorderable sibling window', () => {
    const mixed = budget({
      daily: {
        limit: '10.00',
        accrued: '10.00',
        window_ends_at: '2026-07-13T00:00:00Z'
      },
      monthly: {
        limit: '100',
        accrued: 'unknown',
        window_ends_at: '2026-08-01T00:00:00Z'
      }
    });
    expect(budgetState(mixed)).toBe('exhausted');
  });

  it('is unlimited only when neither window has a limit', () => {
    const open = budget({
      daily: {
        limit: null,
        accrued: '3.00',
        window_ends_at: '2026-07-13T00:00:00Z'
      },
      monthly: {
        limit: null,
        accrued: '3.00',
        window_ends_at: '2026-08-01T00:00:00Z'
      }
    });
    expect(budgetState(open)).toBe('unlimited');
    expect(budgetStateLabel(budgetState(open))).toBe('No cost budget');
  });

  it('reports stored policy ahead of any window comparison', () => {
    const saved = budget({
      enforcement_active: false,
      daily: {
        limit: '1.00',
        accrued: '5.00',
        window_ends_at: '2026-07-13T00:00:00Z'
      }
    });
    expect(budgetState(saved)).toBe('policy');
  });
});

describe('budget state wording', () => {
  it('labels every state', () => {
    expect(budgetStateLabel('policy')).toBe('Saved policy · not enforced');
    expect(budgetStateLabel('exhausted')).toBe('Budget exhausted');
    expect(budgetStateLabel('unknown')).toBe('Budget state unknown');
    expect(budgetStateLabel('within')).toBe('Within budget');
  });

  it('explains only the states that need explaining', () => {
    expect(budgetStateNote('exhausted')).toMatch(/refused until the window/);
    expect(budgetStateNote('policy')).toMatch(/enforcement is inactive/);
    expect(budgetStateNote('unknown')).toMatch(/cannot compare/);
    expect(budgetStateNote('within')).toBeNull();
    expect(budgetStateNote('unlimited')).toBeNull();
  });
});
