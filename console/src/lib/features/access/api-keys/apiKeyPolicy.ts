import {
  routeLimitForm,
  routeLimitsInput,
  type RouteLimitForm
} from '../route-limits/policy';
import { groupNames } from '../route-groups/policy';
import {
  attributionForm,
  attributionInput,
  type AttributionForm
} from '../attribution/policy';
import { keyCIDRs } from './keyNetwork';
import { policyForm, policyInput, type PolicyForm } from '../end-users/policy';
import type {
  ApiKey,
  CreateApiKeyInput,
  UpdateApiKeyInput
} from '$lib/features/access/api-keys/api';
import { dateTimeLocalValue } from '$lib/format';

export type ApiKeyPolicyInput = CreateApiKeyInput & UpdateApiKeyInput;

export type ApiKeyFormState = {
  name: string;
  projectId: string;
  budgetGroupId: string;
  scopes: string[];
  allowedRoutes: string[];
  allowedRouteGroups: string;
  rotationIntervalDays: string;
  allowedCIDRs: string;
  requestsPerMinute: string;
  tokensPerMinute: string;
  maxConcurrency: string;
  dailyCostLimit: string;
  monthlyCostLimit: string;
  weeklyCostLimit: string;
  expiresAt: string;
  allowProviderState: boolean;
  responseMetadata: boolean;
  endUserSource: '' | 'header' | 'native';
  endUserPolicy: PolicyForm;
  routeLimits: RouteLimitForm;
  limitTemplate: string;
  allowedAttributionKeys: string[];
  attributionPolicy: AttributionForm;
  /** Admission classes; empty means normal, and a ceiling of the default. */
  priority: string;
  maxPriority: string;
};

export const admissionPriorities = [
  'critical',
  'high',
  'normal',
  'low'
] as const;
type AdmissionPriority = (typeof admissionPriorities)[number];

export function createApiKeyFormState(
  editing: ApiKey | null = null
): ApiKeyFormState {
  return {
    name: editing?.name ?? '',
    projectId: '',
    budgetGroupId: editing?.budget_group_id ?? '',
    scopes: editing ? [...editing.scopes] : ['inference'],
    allowedRoutes: editing ? [...editing.allowed_routes] : [],
    allowedRouteGroups: (editing?.allowed_route_groups ?? []).join(', '),
    rotationIntervalDays: editing?.rotation_interval_days?.toString() ?? '',
    allowedCIDRs: editing?.allowed_cidrs?.join('\n') ?? '',
    requestsPerMinute: editing?.requests_per_minute?.toString() ?? '',
    tokensPerMinute: editing?.tokens_per_minute?.toString() ?? '',
    maxConcurrency: editing?.max_concurrency?.toString() ?? '',
    dailyCostLimit: editing?.budget.daily.limit ?? '',
    monthlyCostLimit: editing?.budget.monthly.limit ?? '',
    weeklyCostLimit: editing?.budget.weekly?.limit ?? '',
    expiresAt: editing?.expires_at
      ? dateTimeLocalValue(editing.expires_at)
      : '',
    allowProviderState: editing?.allow_provider_state ?? false,
    responseMetadata: editing?.response_metadata ?? false,
    endUserSource: editing?.end_user_source ?? '',
    endUserPolicy: policyForm(editing?.end_user_policy),
    routeLimits: routeLimitForm(editing?.route_limits),
    limitTemplate: editing?.limit_template ?? '',
    attributionPolicy: attributionForm(
      editing
        ? {
            required_attribution_keys: editing.required_attribution_keys ?? [],
            attribution_defaults: editing.attribution_defaults ?? {}
          }
        : null
    ),
    allowedAttributionKeys: editing
      ? [...editing.allowed_attribution_keys]
      : [],
    priority: editing?.priority ?? '',
    maxPriority: editing?.max_priority ?? ''
  };
}

function optionalWholeNumber(value: string): number | null {
  return value ? Number(value) : null;
}

function optionalDecimal(value: string): string | null {
  return value.trim() || null;
}

/**
 * True while the expiry field still shows the edited key's saved expiry. The
 * field holds whole local minutes, so re-sending it would round the stored
 * instant down; an unchanged expiry is left out of the update instead.
 */
export function expiryUnchanged(
  state: ApiKeyFormState,
  editing: ApiKey | null = null
): boolean {
  return Boolean(
    editing?.expires_at &&
    state.expiresAt === dateTimeLocalValue(editing.expires_at)
  );
}

export function buildApiKeyPolicyInput(
  state: ApiKeyFormState,
  editing: ApiKey | null = null
): ApiKeyPolicyInput {
  const input: ApiKeyPolicyInput = {
    name: state.name.trim(),
    project_id: state.projectId || null,
    budget_group_id: state.budgetGroupId || null,
    scopes: state.scopes,
    allowed_routes: state.allowedRoutes,
    allowed_route_groups: groupNames(state.allowedRouteGroups),
    rotation_interval_days: state.rotationIntervalDays.trim()
      ? Number(state.rotationIntervalDays)
      : null,
    allowed_cidrs: keyCIDRs(state.allowedCIDRs),
    requests_per_minute: optionalWholeNumber(state.requestsPerMinute),
    tokens_per_minute: optionalWholeNumber(state.tokensPerMinute),
    max_concurrency: optionalWholeNumber(state.maxConcurrency),
    daily_cost_limit: optionalDecimal(state.dailyCostLimit),
    monthly_cost_limit: optionalDecimal(state.monthlyCostLimit),
    weekly_cost_limit: optionalDecimal(state.weeklyCostLimit),
    expires_at: state.expiresAt
      ? new Date(state.expiresAt).toISOString()
      : null,
    allow_provider_state: state.allowProviderState,
    response_metadata: state.responseMetadata,
    end_user_source: state.endUserSource || null,
    end_user_policy: policyInput(state.endUserPolicy),
    route_limits: routeLimitsInput(state.routeLimits),
    limit_template: state.limitTemplate.trim() || null,
    allowed_attribution_keys: state.allowedAttributionKeys,
    required_attribution_keys:
      attributionInput(state.attributionPolicy)?.required_attribution_keys ??
      [],
    attribution_defaults:
      attributionInput(state.attributionPolicy)?.attribution_defaults ?? {},
    priority: (state.priority || null) as AdmissionPriority | null,
    max_priority: (state.maxPriority || null) as AdmissionPriority | null
  };
  if (expiryUnchanged(state, editing)) delete input.expires_at;
  return input;
}
