import { describe, expect, it } from 'vitest';
import { currentBudgetLimit } from './budgetLimit.js';

describe('current budget limit', () => {
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
});
