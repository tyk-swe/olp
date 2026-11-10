import { expect, it } from 'vitest';
import type { ApiKey } from './api';
import {
  buildApiKeyPolicyInput,
  compileApiKeyPolicy,
  createApiKeyFormState
} from './apiKeyPolicy';
import { validateApiKey } from './keyValidation';

it('checks the default priority against the highest permitted priority', () => {
  expect(
    validateApiKey({ name: 'key', priority: 'critical', maxPriority: 'high' })
  ).toHaveProperty('priority');
  expect(validateApiKey({ name: 'key', maxPriority: 'low' })).toHaveProperty(
    'priority'
  );
  expect(
    validateApiKey({ name: 'key', priority: 'low', maxPriority: 'high' })
  ).not.toHaveProperty('priority');
  expect(
    validateApiKey({ name: 'key', priority: 'high', maxPriority: 'high' })
  ).not.toHaveProperty('priority');
});

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
  const compiled = compileApiKeyPolicy(
    state,
    { kind: 'update', key },
    new Date('2040-01-01')
  );
  expect(compiled.ok).toBe(true);
  if (compiled.ok && compiled.command.kind === 'update') {
    expect(compiled.command.key).toBe(key);
    expect(compiled.command.input).not.toHaveProperty('project_id');
    expect(compiled.command.input).not.toHaveProperty('expires_at');
    expect(compiled.command.input.scopes).toEqual(key.scopes);
  }
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

it('requires a name and limits its length after trimming', () => {
  expect(validateApiKey({ name: '  ' })).toHaveProperty(
    'name',
    'Enter a name.'
  );
  expect(validateApiKey({ name: 'x'.repeat(101) })).toHaveProperty(
    'name',
    'Use 100 characters or fewer.'
  );
  expect(validateApiKey({ name: ` ${'x'.repeat(100)} ` })).toEqual({});
});

it.each(['requestsPerMinute', 'tokensPerMinute', 'maxConcurrency'] as const)(
  'requires a positive whole number for %s when provided',
  (field) => {
    for (const value of [0, -1, 1.5, Number.NaN, Number.POSITIVE_INFINITY]) {
      expect(validateApiKey({ name: 'key', [field]: value })).toHaveProperty(
        field,
        'Use a positive whole number.'
      );
    }
    expect(validateApiKey({ name: 'key', [field]: 1 })).toEqual({});
  }
);

it.each(['dailyCostLimit', 'weeklyCostLimit', 'monthlyCostLimit'] as const)(
  'validates the precision and positivity of %s',
  (field) => {
    for (const value of [
      '0',
      '0.000',
      '-1',
      '.1',
      '1.',
      '1e3',
      '1234567890123',
      '0.1234567890123'
    ]) {
      expect(validateApiKey({ name: 'key', [field]: value })).toHaveProperty(
        field
      );
    }
    for (const value of [
      '',
      '  ',
      ' 0.000000000001 ',
      '123456789012.123456789012'
    ]) {
      expect(validateApiKey({ name: 'key', [field]: value })).toEqual({});
    }
  }
);

it('requires a valid future expiry relative to the supplied clock', () => {
  const now = new Date('2030-01-02T12:00');
  expect(
    validateApiKey({ name: 'key', now, expiresAt: 'invalid' })
  ).toHaveProperty('expiresAt', 'Enter a valid expiry date and time.');
  for (const expiresAt of ['2030-01-01T12:00', '2030-01-02T12:00']) {
    expect(validateApiKey({ name: 'key', now, expiresAt })).toHaveProperty(
      'expiresAt',
      'Choose an expiry in the future.'
    );
  }
  expect(
    validateApiKey({ name: 'key', now, expiresAt: '2030-01-03T12:00' })
  ).toEqual({});
});

it('reports network and limit errors together so the form can correct them', () => {
  expect(
    validateApiKey({
      name: '',
      allowedCIDRs: '192.0.2.1/33',
      requestsPerMinute: 0
    })
  ).toMatchObject({
    name: 'Enter a name.',
    allowedCIDRs: expect.stringContaining('Use IPv4 or IPv6 CIDR ranges'),
    requestsPerMinute: 'Use a positive whole number.'
  });
});

it('compiles only valid project-scoped policies into creation commands', () => {
  const form = {
    ...createApiKeyFormState(),
    name: 'Key',
    allowedRouteGroups: 'research'
  };
  const now = new Date('2030-01-01T00:00:00Z');
  const invalid = compileApiKeyPolicy(form, { kind: 'create' }, now);
  expect(invalid).toMatchObject({
    ok: false,
    formError: 'Route groups require a project-scoped key.'
  });
  const valid = compileApiKeyPolicy(
    { ...form, projectId: 'project' },
    { kind: 'create' },
    now
  );
  expect(valid).toMatchObject({
    ok: true,
    command: {
      kind: 'create',
      input: { project_id: 'project', allowed_route_groups: ['research'] }
    }
  });
});

it('returns field errors for invalid expiry and numeric values before serialization', () => {
  const form = {
    ...createApiKeyFormState(),
    name: 'Key',
    expiresAt: 'invalid',
    requestsPerMinute: '9007199254740993'
  };
  const invalid = compileApiKeyPolicy(
    form,
    { kind: 'create' },
    new Date('2030-01-01T00:00:00Z')
  );
  expect(invalid).toMatchObject({
    ok: false,
    errors: {
      expiresAt: 'Enter a valid expiry date and time.',
      requestsPerMinute: 'Use a positive whole number.'
    }
  });
});
