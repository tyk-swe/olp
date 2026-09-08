import type { Provider } from '$lib/features/providers/api';
import type { ProviderKindCapability } from '$lib/features/providers/models';
import type { RouteDraft } from '$lib/features/routes/api';

export const provider: Provider = {
  id: 'provider-a',
  name: 'Original provider',
  configuration: { kind: 'openai', auth_mode: 'api_key' },
  state: 'draft',
  connector_ready: false,
  pending_activation: true,
  etag: 'v1',
  created_at: '2026-07-12T12:00:00Z',
  updated_at: '2026-07-12T12:00:00Z',
  model_count: 0,
  enabled_model_count: 0,
  capability_count: 0,
  certified_capability_count: 0
};

export const providerSpec: ProviderKindCapability = {
  kind: 'openai',
  label: 'OpenAI',
  description: 'Official OpenAI HTTPS API',
  default_auth_mode: 'api_key',
  auth_modes: [
    { mode: 'api_key', label: 'Stored API key', credential: 'required' }
  ],
  fields: [],
  presets: []
};

export const draft: RouteDraft = {
  id: 'route-a',
  slug: 'original-route',
  state: 'draft',
  etag: 'v1',
  operations: ['generation'],
  overall_timeout_ms: 120000,
  max_attempts: 1,
  targets: [
    {
      id: 'target-a',
      available: true,
      position: 0,
      provider_id: provider.id,
      provider_name: provider.name,
      provider_model: 'test-model',
      provider_model_id: 'model-a',
      priority: 1,
      weight: 100,
      timeout_ms: 60000
    }
  ],
  created_at: '2026-07-12T12:00:00Z',
  updated_at: '2026-07-12T12:00:00Z'
};
