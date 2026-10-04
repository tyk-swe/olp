import type {
  CodeAccount,
  CodeAttempt,
  CodeBudget,
  CodePool,
  CodeRoute
} from '$lib/api/code-mode';
import { errorMessage, fieldIssues, isEtagMismatch } from '$lib/api/http';
import { formatInteger } from '$lib/format';

/** The resource an editor creates, or updates when `current` is set. */
export type CodeEditing =
  | { kind: 'accounts'; current?: CodeAccount }
  | { kind: 'pools'; current?: CodePool }
  | { kind: 'routes'; current?: CodeRoute }
  | { kind: 'budgets'; current?: CodeBudget };

export function tokenCount(value: number | null | undefined): string {
  return value == null ? 'Unknown' : formatInteger(value);
}

export function nativeModels(value: string): string[] {
  const models = value.split(/[\s,]+/).filter(Boolean);
  if (
    !models.length ||
    models.length > 100 ||
    models.some(
      (model) => !/^[A-Za-z0-9][A-Za-z0-9._:/-]{0,199}$/.test(model)
    ) ||
    new Set(models).size !== models.length
  ) {
    throw new Error(
      'Enter 1–100 unique native model identifiers, separated by spaces or commas.'
    );
  }
  return models;
}

export function tokenLimit(value: string): number | null {
  if (!value.trim()) return null;
  if (
    !/^\d+$/.test(value) ||
    !Number.isSafeInteger(Number(value)) ||
    Number(value) < 1
  ) {
    throw new Error(
      'Token limits must be positive whole numbers no greater than 9007199254740991.'
    );
  }
  return Number(value);
}

export function mutationError(error: unknown): string {
  if (isEtagMismatch(error)) {
    return 'This resource changed. Your edits have been kept. Cancel and reopen the latest version before saving again.';
  }
  const fields = fieldIssues(error);
  return fields.length
    ? fields.map((issue) => `${issue.field}: ${issue.message}`).join(' ')
    : errorMessage(error);
}

export function utcWindowStart(value: string): string {
  return new Date(value).toLocaleString('en-US', {
    timeZone: 'UTC',
    timeZoneName: 'short'
  });
}

export function reservationLabel(attempt: CodeAttempt): string {
  if (attempt.state === 'prepared') return 'Pending reservation';
  if (attempt.state === 'uncertain') return 'Retained uncertain reservation';
  if (attempt.state === 'bound_violation')
    return 'Bound violated — budgeted admission blocked';
  return 'Original reservation';
}

export function refusalAdvice(code: string): string {
  if (code.includes('retired'))
    return 'This tree is permanently retired. Start a fresh independent conversation; this identifier cannot be reassigned.';
  if (code.includes('parent') || code.includes('identity'))
    return 'Verify the client conversation and parent identifiers. An unresolved child cannot start an independent binding.';
  if (code.includes('bound'))
    return 'This operation has no proven safe token bound, or exceeded its bound. Only budget-qualified operations can use a hard token budget.';
  if (code.includes('budget'))
    return 'Review UTC token windows, overlapping budgets and retained uncertainty. Missing usage is not zero.';
  if (
    code.includes('account') ||
    code.includes('principal') ||
    code.includes('grant')
  )
    return 'Check the pinned account, grant and serving health. Resume cannot switch accounts; a fresh independent conversation may select another eligible account.';
  if (
    code.includes('operation') ||
    code.includes('unsupported') ||
    code.includes('model')
  )
    return 'Check the qualified client, operation and native model permissions.';
  return 'Check current project, route, pool and key permissions. A stored pin never overrides revoked access.';
}
