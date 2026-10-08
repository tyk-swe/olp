import type { components } from '$lib/api/schema';
import {
  limitFields,
  limitForm,
  limitsError,
  type LimitForm
} from '../budgets/limitForm';
type Budgets = components['schemas']['AttributionBudgets'];
export const costFields = limitFields.filter((field) => !field.numeric);
export type BudgetForm = {
  id: string;
  label: string;
  value: string;
  limits: LimitForm;
}[];
export function budgetForm(budgets?: Budgets | null): BudgetForm {
  return Object.entries(budgets ?? {}).flatMap(([label, values]) =>
    Object.entries(values).map(([value, limits]) => ({
      id: crypto.randomUUID(),
      label,
      value,
      limits: limitForm(limits)
    }))
  );
}
export function budgetInput(form: BudgetForm): Budgets {
  const groups = new Map<
    string,
    [string, components['schemas']['BudgetPolicy']][]
  >();
  for (const row of form) {
    const label = row.label.trim();
    const entries = groups.get(label) ?? [];
    entries.push([
      row.value.trim(),
      Object.fromEntries(
        costFields.map(({ key }) => [key, row.limits[key].trim() || null])
      )
    ]);
    groups.set(label, entries);
  }
  return Object.fromEntries(
    [...groups].map(([label, entries]) => [label, Object.fromEntries(entries)])
  );
}
export function budgetError(form: BudgetForm): string {
  if (form.length > 64) return 'Use at most 64 label/value budgets.';
  const pairs = new Set<string>();
  for (const row of form) {
    const label = row.label.trim();
    const value = row.value.trim();
    const pair = JSON.stringify([label, value]);
    if (
      !/^[A-Za-z][A-Za-z0-9_.-]{0,31}$/.test(label) ||
      !/^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$/.test(value) ||
      pairs.has(pair)
    )
      return 'Enter a valid, unique machine-token label/value pair for each budget.';
    pairs.add(pair);
    const error = limitsError(row.limits);
    if (error) return error;
    if (!costFields.some(({ key }) => row.limits[key].trim()))
      return 'Enter at least one cost cap for each pair, or remove its budget.';
  }
  return '';
}
