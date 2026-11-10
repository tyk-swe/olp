import { validRouteSlug } from '#lib/routeIdentity.js';
import {
  routeLimitForm,
  routeLimitsInput,
  routeLimitsError,
  type RouteLimitForm
} from '../route-limits/policy';
import { groupNames, groupNamesError } from '../route-groups/policy';
import {
  attributionForm,
  attributionInput,
  attributionError,
  type AttributionForm
} from '../attribution/policy';
import { keyCIDRs } from './keyNetwork';
import { validateApiKey, type admissionPriorities } from './keyValidation';
import {
  policyForm,
  policyInput,
  policyError,
  type PolicyForm
} from '../end-users/policy';
import type {
  ApiKey,
  CreateApiKeyInput,
  UpdateApiKeyInput
} from '#lib/features/access/api-keys/api.js';
import { dateTimeLocalValue } from '#lib/format.js';
import type { components } from '#lib/management/schema.js';

export type ApiKeyPolicyInput = CreateApiKeyInput & UpdateApiKeyInput;

export type ApiKeyFormState = {
  name: string;
  projectId: string;
  budgetGroupId: string;
  scopes: string[];
  allowedRoutes: string[];
  allowedToolsets: string;
  allowedAgents: string;
  allowedRouteGroups: string;
  rotationIntervalDays: string;
  allowedCIDRs: string;
  requestsPerMinute: string;
  tokensPerMinute: string;
  maxConcurrency: string;
  runLimits: components['schemas']['WorkflowRunLimits'] | null;
  dailyCostLimit: string;
  monthlyCostLimit: string;
  weeklyCostLimit: string;
  costBasis: string;
  expiresAt: string;
  allowProviderState: boolean;
  responseMetadata: boolean;
  forbidResponseCache: boolean;
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
    allowedToolsets: (editing?.allowed_toolsets ?? []).join(', '),
    allowedAgents: (editing?.allowed_agents ?? []).join(', '),
    allowedRouteGroups: (editing?.allowed_route_groups ?? []).join(', '),
    rotationIntervalDays: editing?.rotation_interval_days?.toString() ?? '',
    allowedCIDRs: editing?.allowed_cidrs?.join('\n') ?? '',
    requestsPerMinute: editing?.requests_per_minute?.toString() ?? '',
    tokensPerMinute: editing?.tokens_per_minute?.toString() ?? '',
    maxConcurrency: editing?.max_concurrency?.toString() ?? '',
    runLimits: editing?.run_limits ?? null,
    dailyCostLimit: editing?.budget.daily.limit ?? '',
    monthlyCostLimit: editing?.budget.monthly.limit ?? '',
    weeklyCostLimit: editing?.budget.weekly?.limit ?? '',
    costBasis:
      (editing?.budget as { cost_basis?: string } | undefined)?.cost_basis ??
      'cost',
    expiresAt: editing?.expires_at
      ? dateTimeLocalValue(editing.expires_at)
      : '',
    allowProviderState: editing?.allow_provider_state ?? false,
    responseMetadata: editing?.response_metadata ?? false,
    forbidResponseCache: editing?.forbid_response_cache ?? false,
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
    allowed_toolsets: groupNames(state.allowedToolsets),
    allowed_agents: groupNames(state.allowedAgents),
    allowed_route_groups: groupNames(state.allowedRouteGroups),
    rotation_interval_days: state.rotationIntervalDays.trim()
      ? Number(state.rotationIntervalDays)
      : null,
    allowed_cidrs: keyCIDRs(state.allowedCIDRs),
    requests_per_minute: optionalWholeNumber(state.requestsPerMinute),
    tokens_per_minute: optionalWholeNumber(state.tokensPerMinute),
    max_concurrency: optionalWholeNumber(state.maxConcurrency),
    run_limits: state.runLimits,
    daily_cost_limit: optionalDecimal(state.dailyCostLimit),
    monthly_cost_limit: optionalDecimal(state.monthlyCostLimit),
    weekly_cost_limit: optionalDecimal(state.weeklyCostLimit),
    cost_basis: (state.costBasis || 'cost') as 'cost' | 'charge',
    expires_at: state.expiresAt
      ? new Date(state.expiresAt).toISOString()
      : null,
    allow_provider_state: state.allowProviderState,
    response_metadata: state.responseMetadata,
    forbid_response_cache: state.forbidResponseCache,
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

export type ApiKeyPolicyContext =
  | { readonly kind: 'create' }
  | { readonly kind: 'update'; readonly key: ApiKey };
export type ApiKeyPolicyCommand =
  | { readonly kind: 'create'; readonly input: CreateApiKeyInput }
  | {
      readonly kind: 'update';
      readonly key: ApiKey;
      readonly input: UpdateApiKeyInput;
    };
export type ApiKeyPolicyResult =
  | { readonly ok: true; readonly command: ApiKeyPolicyCommand }
  | {
      readonly ok: false;
      readonly errors: Record<string, string>;
      readonly formError: string;
    };

export function compileApiKeyPolicy(
  form: ApiKeyFormState,
  context: ApiKeyPolicyContext,
  now: Date
): ApiKeyPolicyResult {
  const editing = context.kind === 'update' ? context.key : null;
  const project = editing ? editing.project_id : form.projectId || null;
  const numericValue = (raw: string): number | undefined =>
    raw ? Number(raw) : undefined;
  const validated = validateApiKey({
    name: form.name,
    allowedCIDRs: form.allowedCIDRs,
    requestsPerMinute: numericValue(form.requestsPerMinute),
    tokensPerMinute: numericValue(form.tokensPerMinute),
    maxConcurrency: numericValue(form.maxConcurrency),
    dailyCostLimit: form.dailyCostLimit,
    monthlyCostLimit: form.monthlyCostLimit,
    weeklyCostLimit: form.weeklyCostLimit,
    expiresAt: expiryUnchanged(form, editing) ? undefined : form.expiresAt,
    priority: form.priority,
    maxPriority: form.maxPriority,
    now
  });
  const numericErrors = Object.fromEntries(
    (['requestsPerMinute', 'tokensPerMinute', 'maxConcurrency'] as const)
      .filter(
        (field) => form[field] && !Number.isSafeInteger(Number(form[field]))
      )
      .map((field) => [field, 'Use a positive whole number.'])
  );
  const errors = { ...validated, ...numericErrors };
  const formError =
    form.rotationIntervalDays.trim() &&
    (!/^\d+$/.test(form.rotationIntervalDays) ||
      Number(form.rotationIntervalDays) < 1 ||
      Number(form.rotationIntervalDays) > 3650)
      ? 'Rotation interval must be a whole number of days from 1 to 3650.'
      : form.limitTemplate &&
          (!project || !validRouteSlug(form.limitTemplate.trim()))
        ? 'Choose a project and a valid limit template name.'
        : form.endUserPolicy.enabled && !form.endUserSource
          ? 'Choose an identifier source before enabling end-user limits.'
          : routeLimitsError(form.routeLimits) ||
            policyError(form.endUserPolicy) ||
            attributionError(form.attributionPolicy) ||
            groupNamesError(groupNames(form.allowedRouteGroups)) ||
            (groupNames(form.allowedRouteGroups).length && !project
              ? 'Route groups require a project-scoped key.'
              : '') ||
            (form.scopes.length ? '' : 'Select at least one scope.');
  if (formError || Object.keys(errors).length)
    return { ok: false, errors, formError };
  const input = buildApiKeyPolicyInput(form, editing);
  if (context.kind === 'create')
    return { ok: true, command: { kind: 'create', input } };
  const { project_id: _project, ...update } = input;
  void _project;
  return {
    ok: true,
    command: { kind: 'update', key: context.key, input: update }
  };
}
