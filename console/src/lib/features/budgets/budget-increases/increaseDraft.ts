import { validated, type Validation } from '#lib/editing/validation.js';
import { limitForm, limitsError } from '../limitForm';
import type { IncreaseInput } from './api';

export type IncreaseDraft = {
  kind: IncreaseInput['target']['kind'];
  resourceId: string;
  route: string;
  digest: string;
  label: string;
  value: string;
  window: IncreaseInput['window'];
  amount: string;
  reason: string;
  expires: string;
};

export type IncreaseField = Exclude<keyof IncreaseDraft, 'label' | 'value'>;

export const increaseServerFields: Readonly<Record<string, IncreaseField>> = {
  'target.kind': 'kind',
  'target.id': 'resourceId',
  'target.route': 'route',
  'target.end_user_digest': 'digest',
  window: 'window',
  amount: 'amount',
  reason: 'reason',
  expires_at: 'expires'
};

export function increaseDraft(): IncreaseDraft {
  return {
    kind: 'api_key',
    resourceId: '',
    route: '',
    digest: '',
    label: '',
    value: '',
    window: 'day',
    amount: '',
    reason: '',
    expires: ''
  };
}

function target(draft: IncreaseDraft): IncreaseInput['target'] {
  const { kind } = draft;
  return {
    kind,
    ...(kind === 'installation' ? {} : { id: draft.resourceId.trim() }),
    ...(kind === 'key_route' ? { route: draft.route.trim() } : {}),
    ...(kind === 'key_end_user' || kind === 'project_end_user'
      ? { end_user_digest: draft.digest.trim() }
      : {}),
    ...(kind === 'attribution'
      ? { label: draft.label.trim(), value: draft.value.trim() }
      : {})
  };
}

function amountError(amount: string) {
  if (!amount) return 'Supply a positive USD amount.';
  return limitsError({ ...limitForm(), daily_cost_limit: amount }) || undefined;
}

export function validateIncrease(
  draft: IncreaseDraft
): Validation<IncreaseInput, IncreaseField> {
  const amount = draft.amount.trim();
  const reason = draft.reason.trim();
  const expires = draft.expires ? new Date(draft.expires) : null;
  return validated(
    {
      amount: amountError(amount),
      reason: reason ? undefined : 'Supply a reason.',
      expires:
        expires && Number.isNaN(expires.getTime())
          ? 'Choose a valid expiry.'
          : undefined
    },
    () => ({
      target: target(draft),
      window: draft.window,
      amount,
      reason,
      expires_at: expires?.toISOString() ?? null
    })
  );
}
