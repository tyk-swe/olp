import { expect, it } from 'vitest';
import type { ApiKey } from './api';
import { buildApiKeyPolicyInput, createApiKeyFormState } from './apiKeyPolicy';

it('includes workflow run limits in a key policy', () => {
  const form = {
    ...createApiKeyFormState(null),
    runLimits: {
      max_iterations: 3,
      max_tool_calls: 0,
      max_concurrent_runs: 2,
      max_run_cost: '0.25'
    }
  };
  expect(buildApiKeyPolicyInput(form).run_limits).toEqual(form.runLimits);
});

it('starts a new key with inference access and no additional permissions or limits', () => {
  const state = createApiKeyFormState(null);
  const input = buildApiKeyPolicyInput(state);
  expect(input).toMatchObject({
    name: '',
    project_id: null,
    budget_group_id: null,
    scopes: ['inference'],
    allowed_routes: [],
    allowed_toolsets: [],
    allowed_agents: [],
    allowed_route_groups: [],
    allowed_cidrs: [],
    requests_per_minute: null,
    tokens_per_minute: null,
    max_concurrency: null,
    daily_cost_limit: null,
    monthly_cost_limit: null,
    weekly_cost_limit: null,
    cost_basis: 'cost',
    expires_at: null,
    allow_provider_state: false,
    response_metadata: false,
    forbid_response_cache: false,
    allowed_attribution_keys: [],
    required_attribution_keys: [],
    attribution_defaults: {}
  });
});

it('serializes chosen key permissions and limits without adding access', () => {
  const state = {
    ...createApiKeyFormState(null),
    name: '  Example key  ',
    projectId: 'project',
    budgetGroupId: 'group',
    scopes: ['inference', 'tools', 'agents'],
    allowedRoutes: ['route-a', 'route-b'],
    allowedToolsets: 'tools-a, tools-b',
    allowedAgents: 'agent-a agent-b',
    allowedRouteGroups: 'group-a, group-b',
    rotationIntervalDays: '30',
    allowedCIDRs: '192.0.2.0/24\n2001:db8::/32',
    requestsPerMinute: '25',
    tokensPerMinute: '500',
    maxConcurrency: '3',
    dailyCostLimit: ' 0.25 ',
    weeklyCostLimit: ' 1.5 ',
    monthlyCostLimit: ' 5 ',
    costBasis: 'charge',
    expiresAt: '2030-01-03T12:45',
    allowProviderState: true,
    responseMetadata: true,
    forbidResponseCache: true,
    limitTemplate: ' standard ',
    allowedAttributionKeys: ['department'],
    attributionPolicy: {
      required: 'department',
      defaults: [{ id: 'row', key: 'department', value: 'research' }]
    },
    priority: 'high',
    maxPriority: 'critical'
  };
  expect(buildApiKeyPolicyInput(state)).toMatchObject({
    name: 'Example key',
    project_id: 'project',
    budget_group_id: 'group',
    scopes: ['inference', 'tools', 'agents'],
    allowed_routes: ['route-a', 'route-b'],
    allowed_toolsets: ['tools-a', 'tools-b'],
    allowed_agents: ['agent-a', 'agent-b'],
    allowed_route_groups: ['group-a', 'group-b'],
    rotation_interval_days: 30,
    allowed_cidrs: ['192.0.2.0/24', '2001:db8::/32'],
    requests_per_minute: 25,
    tokens_per_minute: 500,
    max_concurrency: 3,
    daily_cost_limit: '0.25',
    weekly_cost_limit: '1.5',
    monthly_cost_limit: '5',
    cost_basis: 'charge',
    expires_at: new Date(state.expiresAt).toISOString(),
    allow_provider_state: true,
    response_metadata: true,
    forbid_response_cache: true,
    limit_template: 'standard',
    allowed_attribution_keys: ['department'],
    required_attribution_keys: ['department'],
    attribution_defaults: { department: 'research' },
    priority: 'high',
    max_priority: 'critical'
  });
});

it('preserves configured key permissions, spending limits and expiry when editing', () => {
  const window = { accrued: '0', limit: '2', window_ends_at: '2030-01-01' };
  const key: ApiKey = {
    id: 'key',
    lookup_id: 'lookup',
    name: 'existing key',
    project_id: 'project',
    project_name: 'project name',
    budget_group_id: 'group',
    created_by: 'member',
    created_by_email: 'member@example.test',
    scopes: ['inference', 'agents'],
    allowed_routes: ['route-a'],
    allowed_toolsets: ['tools-a'],
    allowed_agents: ['agent-a'],
    allowed_route_groups: ['group-a'],
    allowed_cidrs: ['192.0.2.0/24'],
    requests_per_minute: 10,
    tokens_per_minute: 100,
    max_concurrency: 2,
    allow_provider_state: true,
    response_metadata: true,
    forbid_response_cache: true,
    allowed_attribution_keys: ['department'],
    required_attribution_keys: ['department'],
    attribution_defaults: { department: 'research' },
    run_limits: { max_iterations: 4, max_tool_calls: 6, max_run_cost: '1' },
    budget: {
      daily: window,
      monthly: window,
      weekly: window,
      unpriced_attempts: 0
    },
    expires_at: '2030-01-03T12:45:36Z',
    etag: 'etag',
    created_at: '2026-01-01',
    regional_limits: {}
  };
  const state = createApiKeyFormState(key);
  const saved = buildApiKeyPolicyInput(state, key);
  expect(saved).toMatchObject({
    name: key.name,
    budget_group_id: key.budget_group_id,
    scopes: key.scopes,
    allowed_routes: key.allowed_routes,
    allowed_toolsets: key.allowed_toolsets,
    allowed_agents: key.allowed_agents,
    allowed_route_groups: key.allowed_route_groups,
    allowed_cidrs: key.allowed_cidrs,
    requests_per_minute: key.requests_per_minute,
    tokens_per_minute: key.tokens_per_minute,
    max_concurrency: key.max_concurrency,
    daily_cost_limit: key.budget.daily.limit,
    monthly_cost_limit: key.budget.monthly.limit,
    weekly_cost_limit: key.budget.weekly?.limit,
    allow_provider_state: true,
    response_metadata: true,
    forbid_response_cache: true,
    allowed_attribution_keys: ['department'],
    required_attribution_keys: ['department'],
    attribution_defaults: { department: 'research' },
    run_limits: key.run_limits
  });
  expect(saved).not.toHaveProperty('expires_at');
  const changed = { ...state, expiresAt: '2031-02-04T03:30' };
  expect(buildApiKeyPolicyInput(changed, key).expires_at).toBe(
    new Date(changed.expiresAt).toISOString()
  );
});

it('opens an older key without optional limits or allowlists for editing', () => {
  const window = { accrued: '0', limit: null, window_ends_at: '2030-01-01' };
  const key: ApiKey = {
    id: 'key',
    lookup_id: 'lookup',
    name: 'existing key',
    project_id: null,
    project_name: null,
    budget_group_id: null,
    created_by: 'member',
    created_by_email: 'member@example.test',
    scopes: ['inference'],
    allowed_routes: [],
    allow_provider_state: false,
    response_metadata: false,
    allowed_attribution_keys: [],
    budget: { daily: window, monthly: window, unpriced_attempts: 0 },
    etag: 'etag',
    created_at: '2026-01-01',
    regional_limits: {}
  };
  expect(buildApiKeyPolicyInput(createApiKeyFormState(key), key)).toMatchObject(
    {
      allowed_toolsets: [],
      allowed_agents: [],
      allowed_route_groups: [],
      allowed_cidrs: [],
      requests_per_minute: null,
      tokens_per_minute: null,
      max_concurrency: null,
      run_limits: null,
      weekly_cost_limit: null,
      required_attribution_keys: [],
      attribution_defaults: {}
    }
  );
});
