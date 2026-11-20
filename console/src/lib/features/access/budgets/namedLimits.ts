import { validRouteSlug } from '#lib/routeIdentity.js';
import type { components } from '#lib/management/schema.js';
import {
  limitForm,
  limitsInput,
  limitsError,
  type LimitForm
} from './limitForm';

type AdmissionLimits = components['schemas']['EndUserLimits'];
type NamedLimits = Record<string, AdmissionLimits>;
export type NamedLimitRow = { id: string; name: string; limits: LimitForm };

export const namedLimitEditors = {
  route: {
    legend: 'Limits by route',
    help: 'Additional limits for this key on individual routes. Overall limits also apply. Subscription routes use rate and concurrency limits without USD budgets.',
    label: 'Route slug',
    item: 'route limit',
    plural: 'routes',
    maxEntries: 100
  },
  template: {
    legend: 'Limit templates',
    help: 'Every referencing key, end-user policy and budget group inherits these ceilings. Inline limits may be stricter. Each boundary keeps its own counters.',
    label: 'Template name',
    item: 'template',
    plural: 'templates',
    maxEntries: 64
  }
} as const;

export type NamedLimitKind = keyof typeof namedLimitEditors;

export function namedLimitRow(
  name = '',
  limits?: AdmissionLimits
): NamedLimitRow {
  return { id: crypto.randomUUID(), name, limits: limitForm(limits) };
}

export function namedLimitForm(value?: NamedLimits | null): NamedLimitRow[] {
  return Object.entries(value ?? {}).map(([name, limits]) =>
    namedLimitRow(name, limits)
  );
}

export function namedLimitsInput(form: NamedLimitRow[]): NamedLimits {
  return Object.fromEntries(
    form.map((row) => [row.name.trim(), limitsInput(row.limits)])
  );
}

export function namedLimitsError(
  form: NamedLimitRow[],
  kind: NamedLimitKind
): string {
  const { maxEntries, plural, label } = namedLimitEditors[kind];
  if (form.length > maxEntries) return `Use at most ${maxEntries} ${plural}.`;
  const names = form.map((row) => row.name.trim());
  if (
    names.some((name) => !validRouteSlug(name)) ||
    new Set(names).size !== names.length
  )
    return `Enter a unique ${label.toLowerCase()} for every entry.`;
  for (const row of form) {
    const error = limitsError(row.limits);
    if (error) return error;
  }
  return '';
}
