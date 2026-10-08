import type { components } from '$lib/api/schema';
import {
  limitForm,
  limitsInput,
  limitsError,
  type LimitForm
} from '../budgets/limitForm';

type LimitTemplates = components['schemas']['LimitTemplates'];
export type TemplateForm = { id: string; name: string; limits: LimitForm }[];
export function templateForm(value?: LimitTemplates | null): TemplateForm {
  return Object.entries(value ?? {}).map(([name, limits]) => ({
    id: crypto.randomUUID(),
    name,
    limits: limitForm(limits)
  }));
}
export function templateInput(form: TemplateForm): LimitTemplates {
  return Object.fromEntries(
    form.map((row) => [row.name.trim(), limitsInput(row.limits)])
  );
}
export function templateError(form: TemplateForm): string {
  if (form.length > 64) return 'Use at most 64 templates.';
  const names = form.map((row) => row.name.trim());
  if (
    names.some((name) => !/^[a-z0-9][a-z0-9._-]{0,99}$/.test(name)) ||
    new Set(names).size !== names.length
  )
    return 'Enter a unique template name for every entry.';
  for (const row of form) {
    const error = limitsError(row.limits);
    if (error) return error;
  }
  return '';
}
