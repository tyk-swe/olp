import type {
  ApiKey,
  CreateApiKeyInput,
  UpdateApiKeyInput
} from '$lib/api/management/api-keys';
import { dateTimeLocalValue } from '$lib/format';

export type ApiKeyPolicyInput = CreateApiKeyInput & UpdateApiKeyInput;

export type ApiKeyFormState = {
  name: string;
  scopes: string[];
  allowedRoutes: string[];
  requestsPerMinute: string;
  tokensPerMinute: string;
  maxConcurrency: string;
  dailyCostLimit: string;
  monthlyCostLimit: string;
  expiresAt: string;
};

export function createApiKeyFormState(
  editing: ApiKey | null = null
): ApiKeyFormState {
  return {
    name: editing?.name ?? '',
    scopes: editing ? [...editing.scopes] : ['inference'],
    allowedRoutes: editing ? [...editing.allowed_routes] : [],
    requestsPerMinute: editing?.requests_per_minute?.toString() ?? '',
    tokensPerMinute: editing?.tokens_per_minute?.toString() ?? '',
    maxConcurrency: editing?.max_concurrency?.toString() ?? '',
    dailyCostLimit: editing?.budget.daily.limit ?? '',
    monthlyCostLimit: editing?.budget.monthly.limit ?? '',
    expiresAt: editing?.expires_at ? dateTimeLocalValue(editing.expires_at) : ''
  };
}

function optionalWholeNumber(value: string): number | null {
  return value ? Number(value) : null;
}

function optionalDecimal(value: string): string | null {
  return value.trim() || null;
}

export function buildApiKeyPolicyInput(
  state: ApiKeyFormState
): ApiKeyPolicyInput {
  return {
    name: state.name.trim(),
    scopes: state.scopes,
    allowed_routes: state.allowedRoutes,
    requests_per_minute: optionalWholeNumber(state.requestsPerMinute),
    tokens_per_minute: optionalWholeNumber(state.tokensPerMinute),
    max_concurrency: optionalWholeNumber(state.maxConcurrency),
    daily_cost_limit: optionalDecimal(state.dailyCostLimit),
    monthly_cost_limit: optionalDecimal(state.monthlyCostLimit),
    expires_at: state.expiresAt ? new Date(state.expiresAt).toISOString() : null
  };
}
