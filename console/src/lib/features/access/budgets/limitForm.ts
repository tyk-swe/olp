import type { components } from '$lib/api/schema';
type AdmissionLimits = components['schemas']['EndUserLimits'];
export const limitFields = [
  { key: 'requests_per_minute', label: 'Requests per minute', numeric: true },
  { key: 'tokens_per_minute', label: 'Tokens per minute', numeric: true },
  { key: 'max_concurrency', label: 'Concurrent requests', numeric: true },
  { key: 'daily_cost_limit', label: 'Daily budget (USD)', numeric: false },
  { key: 'weekly_cost_limit', label: 'Weekly budget (USD)', numeric: false },
  { key: 'monthly_cost_limit', label: 'Monthly budget (USD)', numeric: false }
] as const;
export type LimitForm = Record<(typeof limitFields)[number]['key'], string>;
export function limitForm(limits?: AdmissionLimits): LimitForm {
  return Object.fromEntries(
    limitFields.map(({ key }) => [key, limits?.[key]?.toString() ?? ''])
  ) as LimitForm;
}

export function limitsInput(form: LimitForm): AdmissionLimits {
  return Object.fromEntries(
    limitFields.map(({ key, numeric }) => {
      const value = form[key].trim();
      return [key, value ? (numeric ? Number(value) : value) : null];
    })
  );
}

export function limitsError(limits: LimitForm): string {
  for (const { key, numeric } of limitFields) {
    const value = limits[key].trim();
    if (!value) continue;
    if (numeric) {
      const maximum =
        key === 'tokens_per_minute' ? Number.MAX_SAFE_INTEGER : 2147483647;
      if (!/^\d+$/.test(value) || Number(value) < 1 || Number(value) > maximum)
        return 'Rate and concurrency limits must be positive whole numbers within the supported range.';
    } else if (!/^\d{1,12}(?:\.\d{1,12})?$/.test(value) || !/[1-9]/.test(value))
      return 'Budgets must be positive decimals with at most 12 digits before and after the decimal point.';
  }
  return '';
}
