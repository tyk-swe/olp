import { describe, expect, it } from 'vitest';
import type { ApiKey } from '$lib/api/management/api-keys';
import { buildApiKeyPolicyInput, createApiKeyFormState } from './apiKeyPolicy';

const key = {
  id: '01980000-0000-7000-8000-000000000301',
  lookup_id: 'olp_live_abcd',
  name: 'production SDK',
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
  revoked_at: null,
  rotated_at: null,
  etag: '01980000-0000-7000-8000-000000000302',
  created_by: '01980000-0000-7000-8000-000000000303',
  created_by_email: 'owner@example.com',
  created_at: '2026-07-12T12:00:00Z'
} satisfies ApiKey;

describe('API key form state', () => {
  it('starts a new key with optional limits empty', () => {
    expect(createApiKeyFormState()).toEqual({
      name: '',
      scopes: ['inference'],
      allowedRoutes: [],
      requestsPerMinute: '',
      tokensPerMinute: '',
      maxConcurrency: '',
      dailyCostLimit: '',
      monthlyCostLimit: '',
      expiresAt: ''
    });
  });

  it('preserves exact budget decimals while mapping an existing policy', () => {
    const state = createApiKeyFormState(key);

    expect(state).toMatchObject({
      name: 'production SDK',
      allowedRoutes: ['default'],
      requestsPerMinute: '120',
      dailyCostLimit: '1.250000000001',
      monthlyCostLimit: '20.00'
    });
    expect(buildApiKeyPolicyInput(state)).toMatchObject({
      name: 'production SDK',
      allowed_routes: ['default'],
      requests_per_minute: 120,
      daily_cost_limit: '1.250000000001',
      monthly_cost_limit: '20.00'
    });
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
