import type { BudgetGroup } from '#lib/domain/budget-groups/api.js';
import { formProblem, validated } from '#lib/editing/validation.js';
import { validRouteSlug } from '#lib/routeIdentity.js';
import {
  limitErrors,
  limitFields,
  limitForm,
  limitsInput,
  type LimitForm
} from '../limitForm';

export type BudgetGroupDraft = LimitForm & {
  name: string;
  project_id: string;
  limit_template: string;
};

export type BudgetGroupField = Exclude<keyof BudgetGroupDraft, 'project_id'>;

export const budgetGroupRowServerFields: Readonly<
  Record<string, BudgetGroupField>
> = {
  name: 'name',
  daily_cost_limit: 'daily_cost_limit',
  weekly_cost_limit: 'weekly_cost_limit',
  monthly_cost_limit: 'monthly_cost_limit'
};

export const budgetGroupServerFields: Readonly<
  Record<string, BudgetGroupField>
> = {
  ...budgetGroupRowServerFields,
  cost_basis: 'cost_basis',
  limit_template: 'limit_template',
  requests_per_minute: 'requests_per_minute',
  tokens_per_minute: 'tokens_per_minute',
  max_concurrency: 'max_concurrency'
};

const NO_LIMIT = 'Set a limit or reference a project template.';

export function budgetGroupDraft(group?: BudgetGroup): BudgetGroupDraft {
  return {
    ...limitForm(group),
    name: group?.name ?? '',
    project_id: group?.project_id ?? '',
    limit_template: group?.limit_template ?? ''
  };
}

function setsNoLimit(draft: BudgetGroupDraft) {
  return (
    !draft.limit_template.trim() &&
    !limitFields.some(({ key }) => draft[key].trim())
  );
}

function changes(draft: BudgetGroupDraft) {
  return {
    ...limitsInput(draft),
    name: draft.name.trim(),
    limit_template: draft.limit_template.trim() || null
  };
}

function templateError({ limit_template, project_id }: BudgetGroupDraft) {
  return limit_template &&
    (!project_id || !validRouteSlug(limit_template.trim()))
    ? 'Choose a project and a valid template name.'
    : undefined;
}

export function validateBudgetGroupChange(draft: BudgetGroupDraft) {
  if (setsNoLimit(draft)) return formProblem(NO_LIMIT);
  return validated(limitErrors(draft), () => changes(draft));
}

export function validateNewBudgetGroup(draft: BudgetGroupDraft) {
  if (setsNoLimit(draft)) return formProblem(NO_LIMIT);
  return validated(
    { ...limitErrors(draft), limit_template: templateError(draft) },
    () => ({ ...changes(draft), project_id: draft.project_id || null })
  );
}
