import type { FieldErrors } from '#lib/editing/validation.js';
import type { components } from '#lib/management/schema.js';
type AdmissionLimits = components['schemas']['EndUserLimits'];
export const limitFields = [
  { key: 'requests_per_minute', label: 'Requests per minute', numeric: true },
  { key: 'tokens_per_minute', label: 'Tokens per minute', numeric: true },
  { key: 'max_concurrency', label: 'Concurrent requests', numeric: true },
  { key: 'daily_cost_limit', label: 'Daily budget (USD)', numeric: false },
  { key: 'weekly_cost_limit', label: 'Weekly budget (USD)', numeric: false },
  { key: 'monthly_cost_limit', label: 'Monthly budget (USD)', numeric: false }
] as const;
export type LimitKey = (typeof limitFields)[number]['key'];
export type LimitForm = Record<LimitKey | 'cost_basis', string>;
export const costBasisLabels: Record<
  NonNullable<AdmissionLimits['cost_basis']>,
  string
> = {
  cost: 'Provider cost',
  charge: 'Consumer charge'
};
export function limitForm(limits?: AdmissionLimits): LimitForm {
  return {
    ...Object.fromEntries(
      limitFields.map(({ key }) => [key, limits?.[key]?.toString() ?? ''])
    ),
    cost_basis: limits?.cost_basis ?? 'cost'
  } as LimitForm;
}

export function limitsInput(form: LimitForm): AdmissionLimits {
  return {
    ...Object.fromEntries(
      limitFields.map(({ key, numeric }) => {
        const value = form[key].trim();
        return [key, value ? (numeric ? Number(value) : value) : null];
      })
    ),
    cost_basis: (form.cost_basis || 'cost') as AdmissionLimits['cost_basis']
  };
}

function limitError(
  { key, numeric }: (typeof limitFields)[number],
  value: string
) {
  if (!value) return undefined;
  if (!numeric)
    return /^\d{1,12}(?:\.\d{1,12})?$/.test(value) && /[1-9]/.test(value)
      ? undefined
      : 'Budgets must be positive decimals with at most 12 digits before and after the decimal point.';
  const maximum =
    key === 'tokens_per_minute' ? Number.MAX_SAFE_INTEGER : 2147483647;
  return /^\d+$/.test(value) && Number(value) >= 1 && Number(value) <= maximum
    ? undefined
    : 'Rate and concurrency limits must be positive whole numbers within the supported range.';
}

export function limitErrors(limits: LimitForm): FieldErrors<LimitKey> {
  return Object.fromEntries(
    limitFields.map((field) => [
      field.key,
      limitError(field, limits[field.key].trim())
    ])
  );
}

export function limitsError(limits: LimitForm): string {
  return Object.values(limitErrors(limits)).find(Boolean) ?? '';
}
