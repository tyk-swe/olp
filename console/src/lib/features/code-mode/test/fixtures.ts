import type {
  CodeAccount,
  CodePool,
  CodeRoute,
  CodeBudget,
  CodeBinding,
  CodeAttempt
} from '$lib/api/code-mode';

export const projectId = '01980000-0000-7000-8000-000000000001';
export const secondProjectId = '01980000-0000-7000-8000-000000000002';
export const userId = '01980000-0000-7000-8000-000000000003';
export const keyId = '01980000-0000-7000-8000-000000000004';
export const etag = '01980000-0000-7000-8000-000000000005';
export const account: CodeAccount = {
  id: '01980000-0000-7000-8000-000000000010',
  project_id: projectId,
  provider_id: '01980000-0000-7000-8000-000000000011',
  credential_id: '01980000-0000-7000-8000-000000000012',
  principal: 'fixture-principal',
  name: 'Coding subscription',
  adapter: 'zai_coding',
  enabled: true,
  eligible: true,
  models: ['native-model'],
  health: 'unknown',
  grant_state: 'current',
  allowance: null,
  etag
};
export const pool: CodePool = {
  id: '01980000-0000-7000-8000-000000000020',
  project_id: projectId,
  name: 'Team coding',
  kind: 'shared',
  owner_user_id: null,
  account_ids: [account.id],
  api_key_ids: [keyId],
  etag
};
export const route: CodeRoute = {
  id: '01980000-0000-7000-8000-000000000030',
  project_id: projectId,
  slug: 'team-code',
  pool_id: pool.id,
  enabled: true,
  models: ['native-model'],
  revision_id: '01980000-0000-7000-8000-000000000031',
  revision: 1,
  published_at: '2026-10-01T10:00:00Z',
  adapters: ['zai_coding'],
  etag
};
export const budget: CodeBudget = {
  id: '01980000-0000-7000-8000-000000000040',
  project_id: projectId,
  route_id: null,
  api_key_id: null,
  daily_tokens: 50000,
  monthly_tokens: null,
  enabled: true,
  etag
};
export const binding: CodeBinding = {
  id: '01980000-0000-7000-8000-000000000050',
  root_id: '01980000-0000-7000-8000-000000000050',
  project_id: projectId,
  route_id: route.id,
  api_key_id: keyId,
  account_id: account.id,
  principal: account.principal,
  conversation: 'fixture-conversation',
  parent_id: null,
  created_at: '2026-10-01T10:01:00Z',
  retired_at: null,
  pins: [
    {
      model: 'native-model',
      account_id: account.id,
      principal: account.principal,
      created_at: '2026-10-01T10:01:00Z'
    }
  ]
};
export const attempt: CodeAttempt = {
  upstream_status: null,
  outcome_origin: null,
  outcome: null,
  outcome_observed_at: null,
  id: '01980000-0000-7000-8000-000000000060',
  project_id: projectId,
  route_id: route.id,
  route_revision_id: route.revision_id,
  api_key_id: keyId,
  account_id: account.id,
  binding_id: binding.id,
  operation: 'responses.create',
  model: 'native-model',
  state: 'uncertain',
  reserved_tokens: 4096,
  reported_tokens: null,
  input_tokens: null,
  output_tokens: null,
  cached_tokens: null,
  reasoning_tokens: null,
  bound_evidence: 'fixture-only-bound',
  refusal: null,
  created_at: '2026-10-01T10:02:00Z',
  finished_at: '2026-10-01T10:03:00Z'
};
