import type { components } from '$lib/api/schema';
import {
  limitForm,
  limitsInput,
  limitsError,
  type LimitForm
} from '../budgets/limitForm';

type RouteLimits = components['schemas']['ApiKeyRouteLimits'];
export type RouteLimitForm = { id: string; route: string; limits: LimitForm }[];
export function routeLimitForm(value?: RouteLimits | null): RouteLimitForm {
  return Object.entries(value ?? {}).map(([route, limits]) => ({
    id: crypto.randomUUID(),
    route,
    limits: limitForm(limits)
  }));
}
export function routeLimitsInput(form: RouteLimitForm): RouteLimits {
  return Object.fromEntries(
    form.map((row) => [row.route.trim(), limitsInput(row.limits)])
  );
}
export function routeLimitsError(form: RouteLimitForm): string {
  if (form.length > 100) return 'Use at most 100 routes.';
  const names = form.map((row) => row.route.trim());
  if (
    names.some((name) => !/^[a-z0-9][a-z0-9._-]{0,99}$/.test(name)) ||
    new Set(names).size !== names.length
  )
    return 'Enter a unique route slug for every entry.';
  for (const row of form) {
    const error = limitsError(row.limits);
    if (error) return error;
  }
  return '';
}
