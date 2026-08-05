export const ids = {
  provider: '01980000-0000-7000-8000-000000000101',
  model: '01980000-0000-7000-8000-000000000102',
  credential: '01980000-0000-7000-8000-000000000103',
  draft: '01980000-0000-7000-8000-000000000201',
  target: '01980000-0000-7000-8000-000000000202',
  route: '01980000-0000-7000-8000-000000000203',
  revision: '01980000-0000-7000-8000-000000000204',
  generation: '01980000-0000-7000-8000-000000000205',
  key: '01980000-0000-7000-8000-000000000301',
  user: '01980000-0000-7000-8000-000000000401',
  developer: '01980000-0000-7000-8000-000000000402',
  invitation: '01980000-0000-7000-8000-000000000403',
  session: '01980000-0000-7000-8000-000000000404',
  oidc: '01980000-0000-7000-8000-000000000405'
};

export const now = '2026-07-12T12:00:00Z';
export const sessionOptions = { userId: ids.user, csrfToken: 'csrf-e2e' };

export function providerRecord(
  state = 'draft',
  models: Array<Record<string, unknown>> = [],
  overrides: Record<string, unknown> = {}
) {
  const hasRuntimeRevision = state === 'active';
  const enabledModels = models.filter((model) => model.enabled === true);
  const capabilities = models.flatMap((model) =>
    Array.isArray(model.capabilities) ? model.capabilities : []
  ) as Array<Record<string, unknown>>;
  return {
    id: ids.provider,
    name: 'production-openai',
    kind: 'openai',
    state,
    auth_mode: 'api_key',
    connector_ready: true,
    endpoint: null,
    api_version: null,
    cloud_region: null,
    cloud_project: null,
    deployment: null,
    active_revision: hasRuntimeRevision ? 1 : null,
    pending_activation: false,
    draft_credential_id: ids.credential,
    draft_credential_version: 1,
    runtime_credential_id: hasRuntimeRevision ? ids.credential : null,
    runtime_credential_version: hasRuntimeRevision ? 1 : null,
    last_probe_at: state === 'draft' ? null : now,
    last_probe_status: state === 'draft' ? null : 'succeeded',
    last_probe_detail: state === 'draft' ? null : 'OpenAI reachable',
    etag: '01980000-0000-7000-8000-000000000109',
    created_at: now,
    updated_at: now,
    model_count: models.length,
    enabled_model_count: enabledModels.length,
    capability_count: capabilities.length,
    certified_capability_count: capabilities.filter(
      (capability) => capability.source === 'certified'
    ).length,
    // Test-only state used to serve the separately paginated model endpoint.
    models,
    ...overrides
  };
}

export function withProviderModels(
  provider: ReturnType<typeof providerRecord>,
  models: Array<Record<string, unknown>>,
  overrides: Record<string, unknown> = {}
) {
  const enabledModels = models.filter((model) => model.enabled === true);
  const capabilities = models.flatMap((model) =>
    Array.isArray(model.capabilities) ? model.capabilities : []
  ) as Array<Record<string, unknown>>;
  return {
    ...provider,
    model_count: models.length,
    enabled_model_count: enabledModels.length,
    capability_count: capabilities.length,
    certified_capability_count: capabilities.filter(
      (capability) => capability.source === 'certified'
    ).length,
    models,
    ...overrides
  };
}

export const modelRecord = {
  id: ids.model,
  upstream_model: 'gpt-5.4',
  display_name: 'gpt-5.4',
  enabled: true,
  discovered_at: now,
  capabilities: [
    {
      operation: 'generation',
      surface: 'openai',
      mode: 'streaming',
      source: 'declared'
    },
    {
      operation: 'generation',
      surface: 'openai',
      mode: 'unary',
      source: 'declared'
    }
  ]
};

export const certifiedModelRecord = {
  ...modelRecord,
  capabilities: modelRecord.capabilities.map((capability) => ({
    ...capability,
    source: 'certified',
    certified_at: now
  }))
};
