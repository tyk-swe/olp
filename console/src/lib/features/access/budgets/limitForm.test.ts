import { describe, expect, it } from 'vitest';
import { limitForm, limitsInput } from './limitForm';

describe('limit form cost basis', () => {
  it('defaults to provider cost and round-trips charge', () => {
    expect(limitForm().cost_basis).toBe('cost');
    expect(limitsInput(limitForm()).cost_basis).toBe('cost');
    const charge = limitForm({ cost_basis: 'charge' });
    expect(charge.cost_basis).toBe('charge');
    expect(limitsInput(charge).cost_basis).toBe('charge');
  });
});
