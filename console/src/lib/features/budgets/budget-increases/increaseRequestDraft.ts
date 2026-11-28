import { validated, type Validation } from '#lib/editing/validation.js';
import { limitForm, limitsError } from '../limitForm';
import type { IncreaseRequestInput } from './api';

export type IncreaseRequestField = keyof IncreaseRequestInput;

export const increaseRequestServerFields: Readonly<
  Record<string, IncreaseRequestField>
> = {
  project_id: 'project_id',
  window: 'window',
  amount: 'amount',
  reason: 'reason'
};

export function increaseRequestDraft(): IncreaseRequestInput {
  return { project_id: '', window: 'day', amount: '', reason: '' };
}

function amountError(amount: string) {
  if (!amount) return 'Supply a positive USD amount.';
  return limitsError({ ...limitForm(), daily_cost_limit: amount }) || undefined;
}

export function validateIncreaseRequest(
  draft: IncreaseRequestInput
): Validation<IncreaseRequestInput, IncreaseRequestField> {
  const body = {
    project_id: draft.project_id.trim(),
    window: draft.window,
    amount: draft.amount.trim(),
    reason: draft.reason.trim()
  };
  return validated(
    {
      project_id: body.project_id ? undefined : 'Choose a project.',
      amount: amountError(body.amount),
      reason: body.reason ? undefined : 'Supply a reason.'
    },
    () => body
  );
}
