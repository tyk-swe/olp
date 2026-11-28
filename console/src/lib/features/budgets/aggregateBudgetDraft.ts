import { validated } from '#lib/editing/validation.js';
import type { BudgetPolicy } from './api';
import {
  limitErrors,
  limitForm,
  limitsInput,
  type LimitForm
} from './limitForm';

export type AggregateBudgetField =
  'daily_cost_limit' | 'weekly_cost_limit' | 'monthly_cost_limit';

export type AggregateBudgetDraft = Pick<
  LimitForm,
  AggregateBudgetField | 'cost_basis'
>;

export const aggregateBudgetServerFields: Readonly<
  Record<string, AggregateBudgetField>
> = {
  daily_cost_limit: 'daily_cost_limit',
  weekly_cost_limit: 'weekly_cost_limit',
  monthly_cost_limit: 'monthly_cost_limit'
};

export function aggregateBudgetDraft(
  policy?: BudgetPolicy | null
): AggregateBudgetDraft {
  return {
    cost_basis: policy?.cost_basis ?? 'cost',
    daily_cost_limit: policy?.daily_cost_limit ?? '',
    weekly_cost_limit: policy?.weekly_cost_limit ?? '',
    monthly_cost_limit: policy?.monthly_cost_limit ?? ''
  };
}

export function aggregateBudgetPolicy(
  draft: AggregateBudgetDraft
): BudgetPolicy | null {
  const input = limitsInput({ ...limitForm(), ...draft });
  return input.daily_cost_limit ||
    input.weekly_cost_limit ||
    input.monthly_cost_limit
    ? {
        daily_cost_limit: input.daily_cost_limit,
        monthly_cost_limit: input.monthly_cost_limit,
        weekly_cost_limit: input.weekly_cost_limit,
        cost_basis: input.cost_basis
      }
    : null;
}

export function validateAggregateBudget(draft: AggregateBudgetDraft) {
  return validated(limitErrors({ ...limitForm(), ...draft }), () =>
    aggregateBudgetPolicy(draft)
  );
}
