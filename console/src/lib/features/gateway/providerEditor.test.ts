import { describe, expect, it } from 'vitest';
import type { ProviderKindCapability } from '$lib/api/management/providers';
import {
  activationReady,
  authOptionsFor,
  buildCreateProviderInput,
  buildUpdateProviderInput,
  capabilitiesCertified,
  createProviderDraft,
  hasApiVersion,
  hasCloudProject,
  hasCloudRegion,
  hasCustomEndpoint,
  hasDeployment,
  parseManualModelNames,
  probeReady,
  providerEditValues,
  providerStatus,
  requiresCredential,
  requiresSeedModel,
  validateProviderDraft,
  type ProviderEditValues
} from './providerEditor';

const openAiSpec: ProviderKindCapability = {
  kind: 'openai',
  label: 'OpenAI',
  description: 'Official OpenAI HTTPS API',
  default_auth_mode: 'api_key',
  auth_modes: [{ mode: 'api_key', label: 'Stored API key', credential: 'required' }],
  fields: [{ field: 'model', label: 'Seed model', required: false }]
};
const vertexSpec: ProviderKindCapability = {
  ...openAiSpec,
  kind: 'vertex_ai',
  label: 'Vertex AI',
  default_auth_mode: 'adc',
  auth_modes: [
    { mode: 'adc', label: 'Application Default Credentials', credential: 'forbidden' },
    { mode: 'service_account', label: 'Stored service account JSON', credential: 'required' }
  ],
  fields: [
    { field: 'cloud_project', label: 'Cloud project', required: true },
    { field: 'cloud_region', label: 'Cloud location', required: true },
    { field: 'model', label: 'Probe model', required: true }
  ]
};
const azureSpec: ProviderKindCapability = {
  ...openAiSpec,
  kind: 'azure_openai',
  label: 'Azure OpenAI',
  fields: [
    { field: 'endpoint', label: 'Resource endpoint', required: true },
    { field: 'deployment', label: 'Deployment', required: true },
    { field: 'api_version', label: 'API version', required: true },
    { field: 'model', label: 'Seed model', required: false }
  ]
};
const compatibleSpec: ProviderKindCapability = {
  ...openAiSpec,
  kind: 'openai_compatible',
  label: 'OpenAI-compatible',
  fields: [
    { field: 'endpoint', label: 'HTTPS endpoint', required: true },
    { field: 'model', label: 'Seed model', required: false }
  ]
};
const apiKeyDraft = {
  ...createProviderDraft(openAiSpec),
  name: 'production-openai',
  credential: 'write-only-secret'
};

describe('provider editor capability policy', () => {
  it('derives identity modes and credential guidance from server metadata', () => {
    expect(authOptionsFor(vertexSpec)).toEqual([
      ['adc', 'Application Default Credentials'],
      ['service_account', 'Stored service account JSON']
    ]);
    expect(requiresCredential(vertexSpec, 'service_account')).toBe(true);
    expect(requiresCredential(vertexSpec, 'adc')).toBe(false);
  });

  it('derives field visibility and requirements from server metadata', () => {
    expect(requiresSeedModel(vertexSpec)).toBe(true);
    expect(requiresSeedModel(openAiSpec)).toBe(false);
    expect(hasCustomEndpoint(compatibleSpec)).toBe(true);
    expect(hasCustomEndpoint(openAiSpec)).toBe(false);
    expect(hasCloudRegion(vertexSpec)).toBe(true);
    expect(hasCloudProject(vertexSpec)).toBe(true);
    expect(hasDeployment(azureSpec)).toBe(true);
    expect(hasApiVersion(azureSpec)).toBe(true);
  });

  it('validates required fields declared by capability metadata', () => {
    expect(validateProviderDraft({ ...apiKeyDraft, kind: compatibleSpec.kind }, compatibleSpec)).toBe(
      'OpenAI-compatible requires https endpoint.'
    );
    expect(
      validateProviderDraft(
        { ...apiKeyDraft, kind: vertexSpec.kind, authMode: 'adc', credential: '', model: '' },
        vertexSpec
      )
    ).toBe('Name, Vertex probe model, and the selected identity fields are required.');
  });
});

describe('provider editor API mappings', () => {
  it('trims and maps fields selected by capability metadata', () => {
    expect(
      buildCreateProviderInput(
        {
          ...apiKeyDraft,
          kind: azureSpec.kind,
          name: ' production-azure ',
          model: ' deployment-probe ',
          endpoint: ' https://resource.openai.azure.com ',
          apiVersion: ' 2026-01-01 ',
          deployment: ' chat ',
          cloudRegion: 'ignored',
          cloudProject: 'ignored'
        },
        azureSpec
      )
    ).toEqual({
      name: 'production-azure',
      kind: 'azure_openai',
      credential: 'write-only-secret',
      model: 'deployment-probe',
      endpoint: 'https://resource.openai.azure.com',
      api_version: '2026-01-01',
      cloud_region: null,
      cloud_project: null,
      deployment: 'chat',
      auth_mode: 'api_key',
      display_name: 'production-azure'
    });
  });

  it('never sends fields omitted by capability metadata', () => {
    const values: ProviderEditValues = {
      name: ' Primary OpenAI ',
      endpoint: 'https://api.openai.com/v1/',
      apiVersion: 'ignored',
      cloudRegion: 'ignored',
      cloudProject: 'ignored',
      deployment: 'ignored',
      authMode: 'api_key'
    };
    expect(buildUpdateProviderInput(values, openAiSpec)).toEqual({
      name: 'Primary OpenAI',
      endpoint: null,
      api_version: null,
      cloud_region: null,
      cloud_project: null,
      deployment: null,
      auth_mode: 'api_key'
    });
    expect(
      providerEditValues(
        {
          name: 'Primary OpenAI',
          kind: 'openai',
          endpoint: 'https://api.openai.com/v1/',
          api_version: 'ignored',
          cloud_region: 'ignored',
          cloud_project: 'ignored',
          deployment: 'ignored',
          auth_mode: 'api_key'
        },
        openAiSpec
      )
    ).toEqual({
      name: 'Primary OpenAI',
      endpoint: '',
      apiVersion: '',
      cloudRegion: '',
      cloudProject: '',
      deployment: '',
      authMode: 'api_key'
    });
  });

  it('preserves manual identifier order and ignores blank entries', () => {
    expect(parseManualModelNames(' model-a,\nmodel-b\n, model-c ')).toEqual([
      'model-a',
      'model-b',
      'model-c'
    ]);
  });
});

describe('provider editor activation policy', () => {
  const readyDraft = {
    state: 'draft',
    enabled_model_count: 1,
    capability_count: 2,
    certified_capability_count: 2,
    last_probe_at: '2026-07-12T12:01:00Z',
    last_probe_status: 'succeeded',
    updated_at: '2026-07-12T12:00:00Z'
  };

  it('requires certified capabilities and an ETag-bound successful probe', () => {
    expect(capabilitiesCertified(readyDraft)).toBe(true);
    expect(probeReady(readyDraft)).toBe(true);
    expect(activationReady(readyDraft)).toBe(true);
    expect(activationReady({ ...readyDraft, certified_capability_count: 1 })).toBe(false);
    expect(activationReady({ ...readyDraft, last_probe_at: '2026-07-12T11:59:00Z' })).toBe(false);
    expect(activationReady({ ...readyDraft, state: 'active' })).toBe(false);
  });

  it('labels pending changes before active and draft states', () => {
    expect(providerStatus({ ...readyDraft, active_revision: 2, pending_activation: true })).toBe(
      'revision 2 live · changes pending'
    );
    expect(providerStatus({ ...readyDraft, active_revision: 2, pending_activation: false })).toBe(
      'revision 2 active'
    );
    expect(providerStatus({ ...readyDraft, active_revision: null, pending_activation: false })).toBe(
      'draft'
    );
  });
});
