import { describe, expect, it } from 'vitest';
import type { ApiKey } from '$lib/features/access/api-keys/api';
import {
  buildApiKeyPolicyInput,
  expiryUnchanged,
  createApiKeyFormState
} from '$lib/features/access/api-keys/apiKeyPolicy';

const key = {
  id: '01980000-0000-7000-8000-000000000301',
  lookup_id: 'olp_live_abcd',
  name: 'production SDK',
  project_id: null,
  project_name: null,
  budget_group_id: null,
  scopes: ['inference'],
  allowed_routes: ['default'],
  requests_per_minute: 120,
  tokens_per_minute: 24_000,
  max_concurrency: 8,
  budget: {
    daily: {
      limit: '1.250000000001',
      accrued: '0.125',
      window_ends_at: '2026-07-13T00:00:00Z'
    },
    monthly: {
      limit: '20.00',
      accrued: '4.75',
      window_ends_at: '2026-08-01T00:00:00Z'
    },
    unpriced_attempts: 2
  },
  expires_at: '2027-01-01T12:30:00Z',
  allow_provider_state: false,
  response_metadata: true,
  allowed_attribution_keys: ['team'],
  revoked_at: null,
  rotated_at: null,
  etag: '01980000-0000-7000-8000-000000000302',
  created_by: '01980000-0000-7000-8000-000000000303',
  created_by_email: 'owner@example.com',
  created_at: '2026-07-12T12:00:00Z'
} satisfies ApiKey;

describe('API key form state', () => {
  it('round-trips the end-user source and explicitly clears it', () => {
    const state = createApiKeyFormState({ ...key, end_user_source: 'header' });
    expect(state.endUserSource).toBe('header');
    expect(buildApiKeyPolicyInput(state).end_user_source).toBe('header');
    state.endUserSource = 'native';
    expect(buildApiKeyPolicyInput(state).end_user_source).toBe('native');
    state.endUserSource = '';
    expect(buildApiKeyPolicyInput(state).end_user_source).toBeNull();
  });

  it('starts a new key with optional limits empty', () => {
    expect(createApiKeyFormState()).toEqual({
      name: '',
      projectId: '',
      budgetGroupId: '',
      scopes: ['inference'],
      allowedRoutes: [],
      allowedRouteGroups: '',
      rotationIntervalDays: '',
      allowedCIDRs: '',
      requestsPerMinute: '',
      tokensPerMinute: '',
      maxConcurrency: '',
      dailyCostLimit: '',
      monthlyCostLimit: '',
      weeklyCostLimit: '',
      expiresAt: '',
      allowProviderState: false,
      responseMetadata: false,
      endUserSource: '',
      routeLimits: [],
      limitTemplate: '',
      endUserPolicy: {
        enabled: false,
        limitTemplate: '',
        defaults: {
          requests_per_minute: '',
          tokens_per_minute: '',
          max_concurrency: '',
          daily_cost_limit: '',
          weekly_cost_limit: '',
          monthly_cost_limit: ''
        },
        overrides: [],
        blocked: ''
      },
      allowedAttributionKeys: [],
      attributionPolicy: { required: '', defaults: [] },
      priority: '',
      maxPriority: ''
    });
  });

  it('preserves exact budget decimals while mapping an existing policy', () => {
    const state = createApiKeyFormState(key);

    expect(state).toMatchObject({
      name: 'production SDK',
      allowedRoutes: ['default'],
      allowedAttributionKeys: ['team'],
      responseMetadata: true,
      requestsPerMinute: '120',
      dailyCostLimit: '1.250000000001',
      monthlyCostLimit: '20.00'
    });
    expect(buildApiKeyPolicyInput(state)).toMatchObject({
      name: 'production SDK',
      allowed_routes: ['default'],
      allowed_attribution_keys: ['team'],
      response_metadata: true,
      requests_per_minute: 120,
      daily_cost_limit: '1.250000000001',
      monthly_cost_limit: '20.00'
    });
  });

  it('sends the response metadata opt-in as an explicit boolean', () => {
    const state = createApiKeyFormState(key);
    state.responseMetadata = false;
    expect(buildApiKeyPolicyInput(state, key).response_metadata).toBe(false);

    const created = createApiKeyFormState();
    expect(buildApiKeyPolicyInput(created).response_metadata).toBe(false);
    created.responseMetadata = true;
    expect(buildApiKeyPolicyInput(created).response_metadata).toBe(true);
  });

  it('leaves an untouched expiry out instead of rounding it to the minute', () => {
    const editing = { ...key, expires_at: '2027-01-01T12:30:45.123Z' };
    const state = createApiKeyFormState(editing);
    state.name = 'renamed';

    const input = buildApiKeyPolicyInput(state, editing);
    expect(input).not.toHaveProperty('expires_at');
    expect(expiryUnchanged(state, editing)).toBe(true);
  });

  it('sends an edited expiry as the chosen instant', () => {
    const editing = { ...key, expires_at: '2027-01-01T12:30:45.123Z' };
    const state = createApiKeyFormState(editing);
    state.expiresAt = '2027-02-01T09:15';

    expect(buildApiKeyPolicyInput(state, editing).expires_at).toBe(
      new Date('2027-02-01T09:15').toISOString()
    );
  });

  it('clears an expiry the user removed', () => {
    const editing = { ...key, expires_at: '2027-01-01T12:30:45.123Z' };
    const state = createApiKeyFormState(editing);
    state.expiresAt = '';

    expect(buildApiKeyPolicyInput(state, editing).expires_at).toBeNull();
  });

  it('submits blank optional limits as explicit nulls', () => {
    const state = createApiKeyFormState();
    state.name = 'unlimited';

    expect(buildApiKeyPolicyInput(state)).toMatchObject({
      daily_cost_limit: null,
      monthly_cost_limit: null,
      requests_per_minute: null,
      tokens_per_minute: null,
      max_concurrency: null,
      expires_at: null
    });
  });
});

it('round-trips and clears client network restrictions', () => {
  const editing = { ...key, allowed_cidrs: ['192.0.2.0/24', '2001:db8::/32'] };
  const state = createApiKeyFormState(editing);
  expect(state.allowedCIDRs).toBe('192.0.2.0/24\n2001:db8::/32');
  expect(buildApiKeyPolicyInput(state, editing).allowed_cidrs).toEqual(
    editing.allowed_cidrs
  );
  state.allowedCIDRs = '';
  expect(buildApiKeyPolicyInput(state, editing).allowed_cidrs).toEqual([]);
});

describe('route group references', () => {
  it('loads, writes and explicitly clears group references', () => {
    const form = createApiKeyFormState({
      ...key,
      allowed_route_groups: ['production', 'backup']
    });
    expect(form.allowedRouteGroups).toBe('production, backup');
    expect(buildApiKeyPolicyInput(form).allowed_route_groups).toEqual([
      'production',
      'backup'
    ]);
    form.allowedRouteGroups = '';
    expect(buildApiKeyPolicyInput(form).allowed_route_groups).toEqual([]);
  });
});
