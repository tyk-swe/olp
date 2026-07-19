import { describe, expect, it } from 'vitest';
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
  supportsManualModelDeclaration,
  validateProviderDraft,
  type ProviderEditValues
} from './providerEditor';

const apiKeyDraft = {
  ...createProviderDraft(),
  name: 'production-openai',
  credential: 'write-only-secret'
};

describe('provider editor connector policy', () => {
  it('selects only connector-supported identity modes', () => {
    expect(authOptionsFor('open_ai')).toEqual([['api_key', 'Stored API key']]);
    expect(authOptionsFor('vertex_ai')).toEqual([
      ['adc', 'Application Default Credentials'],
      ['service_account', 'Stored service account JSON']
    ]);
    expect(authOptionsFor('bedrock')).toEqual([
      ['default_chain', 'AWS default chain'],
      ['static', 'Stored static AWS credential']
    ]);
    expect(requiresCredential('api_key')).toBe(true);
    expect(requiresCredential('service_account')).toBe(true);
    expect(requiresCredential('adc')).toBe(false);
    expect(requiresCredential('default_chain')).toBe(false);
  });

  it('keeps connector context fields limited to their owning connectors', () => {
    expect(requiresSeedModel('vertex_ai')).toBe(true);
    expect(requiresSeedModel('open_ai')).toBe(false);
    expect(hasCustomEndpoint('open_ai_compatible')).toBe(true);
    expect(hasCustomEndpoint('azure_open_ai')).toBe(true);
    expect(hasCustomEndpoint('open_ai')).toBe(false);
    expect(hasCloudRegion('vertex_ai')).toBe(true);
    expect(hasCloudRegion('bedrock')).toBe(true);
    expect(hasCloudRegion('gemini')).toBe(false);
    expect(hasCloudProject('vertex_ai')).toBe(true);
    expect(hasCloudProject('bedrock')).toBe(false);
    expect(hasDeployment('azure_open_ai')).toBe(true);
    expect(hasApiVersion('azure_open_ai')).toBe(true);
    expect(hasDeployment('open_ai_compatible')).toBe(false);
    expect(hasApiVersion('open_ai_compatible')).toBe(false);
    expect(supportsManualModelDeclaration('open_ai_compatible')).toBe(true);
    expect(supportsManualModelDeclaration('bedrock')).toBe(true);
    expect(supportsManualModelDeclaration('open_ai')).toBe(false);
  });

  it('enforces connector-specific creation requirements', () => {
    expect(validateProviderDraft({ ...apiKeyDraft, kind: 'open_ai_compatible' })).toBe(
      'An HTTPS endpoint is required for an OpenAI-compatible provider.'
    );
    expect(
      validateProviderDraft({
        ...apiKeyDraft,
        kind: 'vertex_ai',
        authMode: 'adc',
        credential: '',
        model: ''
      })
    ).toBe('Name, Vertex probe model, and the selected identity fields are required.');
    expect(
      validateProviderDraft({
        ...apiKeyDraft,
        kind: 'bedrock',
        authMode: 'default_chain',
        credential: ''
      })
    ).toBe('AWS Bedrock requires a cloud region.');
    expect(validateProviderDraft({ ...apiKeyDraft, kind: 'azure_open_ai' })).toBe(
      'Azure OpenAI requires its resource endpoint, deployment, and API version.'
    );
  });
});

describe('provider editor API mappings', () => {
  it('trims and maps Azure creation fields without leaking other connector context', () => {
    expect(
      buildCreateProviderInput({
        ...apiKeyDraft,
        kind: 'azure_open_ai',
        name: ' production-azure ',
        model: ' deployment-probe ',
        endpoint: ' https://resource.openai.azure.com ',
        apiVersion: ' 2026-01-01 ',
        deployment: ' chat ',
        cloudRegion: 'ignored',
        cloudProject: 'ignored'
      })
    ).toEqual({
      name: 'production-azure',
      kind: 'azure_open_ai',
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

  it('never sends a native provider endpoint or cloud context back to the API', () => {
    const values: ProviderEditValues = {
      name: ' Primary OpenAI ',
      endpoint: 'https://api.openai.com/v1/',
      apiVersion: 'ignored',
      cloudRegion: 'ignored',
      cloudProject: 'ignored',
      deployment: 'ignored',
      authMode: 'api_key'
    };

    expect(buildUpdateProviderInput({ kind: 'open_ai' }, values)).toEqual({
      name: 'Primary OpenAI',
      endpoint: null,
      api_version: null,
      cloud_region: null,
      cloud_project: null,
      deployment: null,
      auth_mode: 'api_key'
    });
    expect(
      providerEditValues({
        name: 'Primary OpenAI',
        kind: 'open_ai',
        endpoint: 'https://api.openai.com/v1/',
        api_version: 'ignored',
        cloud_region: 'ignored',
        cloud_project: 'ignored',
        deployment: 'ignored',
        auth_mode: 'api_key'
      })
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
    invalid_enabled_model_count: 0,
    probe_ready: true,
    last_probe_at: '2026-07-12T12:01:00Z',
    last_probe_status: 'succeeded'
  };

  it('requires certified enabled capabilities and a successful context-bound probe', () => {
    expect(capabilitiesCertified(readyDraft)).toBe(true);
    expect(probeReady(readyDraft)).toBe(true);
    expect(activationReady(readyDraft)).toBe(true);
    expect(activationReady({ ...readyDraft, invalid_enabled_model_count: 1 })).toBe(false);
    expect(activationReady({ ...readyDraft, probe_ready: false })).toBe(false);
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
