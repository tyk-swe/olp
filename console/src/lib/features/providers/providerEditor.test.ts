import { describe, expect, it } from 'vitest';
import type { ProviderKindCapability } from '$lib/features/providers/api/models';
import {
  activationReady,
  authOptionsFor,
  buildCreateProviderInput,
  buildUpdateProviderInput,
  capabilitiesCertified,
  capabilityLimitReached,
  certificationPrerequisiteReady,
  createProviderDraft,
  disableNotice,
  DISABLED_EDIT_NOTE,
  emptyProviderOptions,
  hasApiVersion,
  hasCloudProject,
  hasCloudRegion,
  hasCustomEndpoint,
  hasDeployment,
  MAX_REVIEWED_CAPABILITIES,
  parseManualModelNames,
  probeReady,
  probeSummary,
  providerDisabled,
  providerEditValues,
  providerStatus,
  providerStatusTone,
  requiresCredential,
  requiresGrant,
  probeModelPrompt,
  requiresProbeModel,
  requiresSeedModel,
  selectPluginProfile,
  selectProviderPreset,
  setProviderDraftKind,
  validateProviderDraft,
  type ProviderEditValues
} from '$lib/features/providers/providerEditor';
import {
  pluginSpec,
  referenceProfile
} from '$lib/features/providers/test/pluginFixtures';

const openAiSpec: ProviderKindCapability = {
  kind: 'openai',
  label: 'OpenAI',
  description: 'Official OpenAI HTTPS API',
  default_auth_mode: 'api_key',
  auth_modes: [
    { mode: 'api_key', label: 'Stored API key', credential: 'required' }
  ],
  fields: [{ field: 'model', label: 'Seed model', required: false }],
  presets: []
};
const vertexSpec: ProviderKindCapability = {
  ...openAiSpec,
  kind: 'vertex_ai',
  label: 'Vertex AI',
  default_auth_mode: 'adc',
  auth_modes: [
    {
      mode: 'adc',
      label: 'Application Default Credentials',
      credential: 'forbidden'
    },
    {
      mode: 'service_account',
      label: 'Stored service account JSON',
      credential: 'required'
    }
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
const sagemakerSpec: ProviderKindCapability = {
  ...openAiSpec,
  kind: 'sagemaker',
  label: 'Amazon SageMaker AI',
  default_auth_mode: 'default_chain',
  auth_modes: [
    {
      mode: 'default_chain',
      label: 'AWS credential chain',
      credential: 'forbidden'
    }
  ],
  fields: [
    { field: 'cloud_region', label: 'Region', required: true },
    { field: 'model', label: 'Probe model', required: true }
  ]
};
const compatibleSpec: ProviderKindCapability = {
  ...openAiSpec,
  kind: 'openai_compatible',
  label: 'OpenAI-compatible',
  fields: [
    { field: 'endpoint', label: 'HTTPS endpoint', required: true },
    { field: 'model', label: 'Seed model', required: false }
  ],
  presets: [
    {
      id: 'groq',
      label: 'Groq',
      description:
        "Low-latency inference through Groq's OpenAI-compatible API.",
      endpoint: 'https://api.groq.com/openai/v1',
      auth_mode: 'api_key',
      maintainer: 'Groq',
      documentation_label: 'OpenAI Compatibility',
      documentation_url: 'https://console.groq.com/docs/openai',
      discovery: true,
      placeholder: false,
      profile_id: null,
      profile_revision: null
    },
    {
      id: 'exact',
      label: 'Exact vendor',
      description: 'A vendor whose contract is the Chat Completions dialect.',
      endpoint: 'https://api.exact.test/v1',
      auth_mode: 'api_key',
      maintainer: 'Exact',
      documentation_label: 'Exact API',
      documentation_url: 'https://docs.exact.test',
      discovery: false,
      placeholder: false,
      profile_id: 'compatible-chat',
      profile_revision: '1'
    }
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
    expect(
      validateProviderDraft(
        { ...apiKeyDraft, kind: compatibleSpec.kind },
        compatibleSpec
      )
    ).toBe('OpenAI-compatible requires https endpoint.');
    expect(
      validateProviderDraft(
        {
          ...apiKeyDraft,
          kind: vertexSpec.kind,
          authMode: 'adc',
          credential: '',
          model: ''
        },
        vertexSpec
      )
    ).toBe('Vertex AI requires cloud project, cloud location, probe model.');
  });

  it('names the missing name and credential alongside required fields', () => {
    expect(
      validateProviderDraft(
        { ...apiKeyDraft, name: '  ', credential: '' },
        openAiSpec
      )
    ).toBe('OpenAI requires name, credential.');
    expect(
      validateProviderDraft(
        {
          ...apiKeyDraft,
          kind: vertexSpec.kind,
          authMode: 'service_account',
          credential: '',
          cloudProject: 'demo-project',
          cloudRegion: 'us-central1',
          model: ''
        },
        vertexSpec
      )
    ).toBe('Vertex AI requires probe model, credential.');
  });

  it('preserves caller serving mode and the separate operator probe credential', () => {
    const draft = createProviderDraft(compatibleSpec);
    draft.name = 'Caller connection';
    draft.credential = 'operator-probe';
    draft.document!.set(['credential_source'], 'caller');
    const input = buildCreateProviderInput(draft, compatibleSpec);
    expect(input.configuration.credential_source).toBe('caller');
    expect(input.credential).toBe('operator-probe');
    const edit = providerEditValues(
      { name: draft.name, configuration: input.configuration },
      compatibleSpec
    );
    expect(
      buildUpdateProviderInput(edit, compatibleSpec).configuration
        .credential_source
    ).toBe('caller');
    edit.document!.set(['credential_source'], 'operator');
    expect(
      buildUpdateProviderInput(edit, compatibleSpec).configuration
        .credential_source
    ).toBe('operator');
  });

  it('resolves a preset to ordinary compatible-provider fields', () => {
    const draft = {
      ...createProviderDraft(compatibleSpec),
      name: 'groq-production',
      credential: 'write-only-secret'
    };
    expect(selectProviderPreset(draft, compatibleSpec, 'groq')).toEqual(
      compatibleSpec.presets[0]
    );
    expect(draft).toMatchObject({
      presetId: 'groq',
      endpoint: 'https://api.groq.com/openai/v1',
      authMode: 'api_key'
    });
    const input = buildCreateProviderInput(draft, compatibleSpec);
    expect(input.configuration).toMatchObject({
      kind: 'openai_compatible',
      endpoint: 'https://api.groq.com/openai/v1',
      auth_mode: 'api_key'
    });
    expect(input.configuration.options?.vendor_id).toBe('groq');
    expect(input).not.toHaveProperty('preset_id');

    expect(selectProviderPreset(draft, compatibleSpec, '')).toBeNull();
    expect(draft).toMatchObject({
      presetId: '',
      endpoint: '',
      authMode: 'api_key'
    });
  });

  it('selects a preset profile only while the operator has not chosen another', () => {
    const draft = createProviderDraft(compatibleSpec);
    selectProviderPreset(draft, compatibleSpec, 'exact');
    expect(draft).toMatchObject({
      profileId: 'compatible-chat',
      profileRevision: '1'
    });
    selectProviderPreset(draft, compatibleSpec, 'groq');
    expect(draft).toMatchObject({ profileId: '', profileRevision: '' });

    draft.profileId = 'compatible-responses';
    draft.profileRevision = '1';
    selectProviderPreset(draft, compatibleSpec, 'exact');
    expect(draft).toMatchObject({
      profileId: 'compatible-responses',
      profileRevision: '1'
    });
  });

  it('requires a probe model for a preset whose upstream lists no models', () => {
    const draft = createProviderDraft(compatibleSpec);
    selectProviderPreset(draft, compatibleSpec, 'groq');
    expect(requiresProbeModel(draft, compatibleSpec)).toBe(false);
    selectProviderPreset(draft, compatibleSpec, 'exact');
    expect(requiresProbeModel(draft, compatibleSpec)).toBe(true);
  });

  it('requires a probe model for a kind whose vendor lists no models', () => {
    const vendors = [
      { id: 'amazon-sagemaker', discovery: false },
      { id: 'openai', discovery: true }
    ];
    const sagemaker = {
      kind: 'sagemaker' as const,
      presetId: '',
      options: { ...emptyProviderOptions(), vendor_id: 'amazon-sagemaker' }
    };
    const openai = {
      kind: 'openai' as const,
      presetId: '',
      options: { ...emptyProviderOptions(), vendor_id: 'openai' }
    };
    expect(requiresProbeModel(sagemaker, openAiSpec, undefined, vendors)).toBe(
      true
    );
    expect(requiresProbeModel(openai, openAiSpec, undefined, vendors)).toBe(
      false
    );
    expect(requiresProbeModel(sagemaker, openAiSpec)).toBe(false);
    expect(probeModelPrompt('sagemaker')).toEqual({
      label: 'SageMaker endpoint',
      placeholder: 'endpoint or endpoint/inference-component'
    });
    expect(probeModelPrompt('openai').label).toBe('Probe model');
  });

  it('requires a SageMaker endpoint before creating a draft with the default vendor', () => {
    const draft = createProviderDraft(sagemakerSpec);
    draft.name = 'SageMaker';
    draft.cloudRegion = 'us-east-1';
    expect(draft.presetId).toBe('');
    expect(draft.options?.vendor_id).toBeFalsy();
    expect(requiresProbeModel(draft, sagemakerSpec)).toBe(true);
    expect(validateProviderDraft(draft, sagemakerSpec)).toBe(
      'Amazon SageMaker AI requires probe model.'
    );
    draft.model = 'endpoint/inference-component';
    expect(validateProviderDraft(draft, sagemakerSpec)).toBeNull();
    expect(buildCreateProviderInput(draft, sagemakerSpec).model).toBe(
      'endpoint/inference-component'
    );
  });

  it('clears the console-only preset selection when provider kind changes', () => {
    const draft = createProviderDraft(compatibleSpec);
    selectProviderPreset(draft, compatibleSpec, 'groq');

    setProviderDraftKind(draft, compatibleSpec.kind);
    expect(draft.presetId).toBe('groq');

    setProviderDraftKind(draft, azureSpec.kind);
    draft.endpoint = 'https://resource.openai.azure.com';
    setProviderDraftKind(draft, compatibleSpec.kind);

    expect(draft).toMatchObject({
      kind: 'openai_compatible',
      presetId: '',
      endpoint: ''
    });
  });

  it('resets every connector-specific field when the kind changes', () => {
    const draft = createProviderDraft(azureSpec);
    Object.assign(draft, {
      name: 'azure-production',
      endpoint: 'https://resource.openai.azure.com',
      apiVersion: '2026-01-01',
      cloudRegion: 'eastus',
      cloudProject: 'analytics',
      deployment: 'gpt-5-4',
      model: 'gpt-5.4',
      credential: 'azure-secret'
    });

    setProviderDraftKind(draft, compatibleSpec.kind);

    expect(draft).toMatchObject({
      kind: 'openai_compatible',
      presetId: '',
      endpoint: '',
      apiVersion: '',
      cloudRegion: '',
      cloudProject: '',
      deployment: '',
      model: '',
      // A secret typed for Azure must never be submitted as the compatible
      // endpoint's key.
      credential: ''
    });
    // The provider name is the operator's label, not connector context.
    expect(draft.name).toBe('azure-production');
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
      project_id: null,
      credential: 'write-only-secret',
      model: 'deployment-probe',
      display_name: 'production-azure',
      configuration: {
        options: {
          vendor_id: null,
          limits: null,
          credential_headers: [],
          parameter_defaults: {},
          models: {}
        },
        kind: 'azure_openai',
        endpoint: 'https://resource.openai.azure.com',
        api_version: '2026-01-01',
        cloud_region: null,
        cloud_project: null,
        deployment: 'chat',
        auth_mode: 'api_key'
      }
    });
  });

  it('merges creation options while applying the selected preset and header input', () => {
    const input = buildCreateProviderInput(
      {
        ...createProviderDraft(compatibleSpec),
        name: 'Compatible',
        endpoint: 'https://api.groq.com/openai/v1',
        credential: '  write-only-secret\n',
        presetId: 'groq',
        credentialHeaders: ' X-API-Key,\n , X-Secondary-Key \n',
        options: {
          ...emptyProviderOptions(),
          vendor_id: 'previous-vendor',
          credential_headers: ['Previous-Key'],
          parameter_defaults: { temperature: 0.2 }
        }
      },
      compatibleSpec
    );

    expect(input.credential).toBe('  write-only-secret\n');
    expect(input.configuration.options).toEqual({
      vendor_id: 'groq',
      limits: null,
      credential_headers: ['X-API-Key', 'X-Secondary-Key'],
      parameter_defaults: { temperature: 0.2 },
      models: {}
    });
    const payload = JSON.parse(JSON.stringify(input));
    expect(payload).not.toHaveProperty('model');
    expect(payload).not.toHaveProperty('display_name');
  });

  it('omits credentials when the authentication mode forbids them', () => {
    const input = buildCreateProviderInput(
      {
        ...createProviderDraft(vertexSpec),
        name: 'Vertex',
        cloudProject: ' production ',
        cloudRegion: ' us-central1 ',
        model: 'gemini-2.5-flash',
        credential: 'previous-secret'
      },
      vertexSpec
    );

    expect(input.configuration).toMatchObject({
      auth_mode: 'adc',
      cloud_project: 'production',
      cloud_region: 'us-central1'
    });
    expect(JSON.parse(JSON.stringify(input))).not.toHaveProperty('credential');
  });

  it('preserves update options without applying creation defaults', () => {
    const values: ProviderEditValues = {
      ...createProviderDraft(compatibleSpec),
      name: 'Compatible',
      endpoint: ' ',
      options: {
        ...emptyProviderOptions(),
        vendor_id: 'custom-vendor',
        credential_headers: ['X-API-Key'],
        parameter_defaults: { temperature: 0.4 }
      }
    };

    const input = buildUpdateProviderInput(values, compatibleSpec);
    expect(input.configuration.endpoint).toBeNull();
    expect(input.configuration.options).toEqual(values.options);
    expect(
      buildUpdateProviderInput(
        { ...values, options: undefined },
        compatibleSpec
      ).configuration.options
    ).toBeUndefined();
  });

  it('filters form inputs the connector kind does not use while preserving complete loaded documents', () => {
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
      configuration: {
        kind: 'openai',
        endpoint: null,
        api_version: null,
        cloud_region: null,
        cloud_project: null,
        deployment: null,
        auth_mode: 'api_key'
      }
    });
    expect(
      providerEditValues(
        {
          name: 'Primary OpenAI',
          configuration: {
            kind: 'openai',
            endpoint: 'https://api.openai.com/v1/',
            api_version: 'ignored',
            cloud_region: 'ignored',
            cloud_project: 'ignored',
            deployment: 'ignored',
            auth_mode: 'api_key'
          }
        },
        openAiSpec
      )
    ).toEqual({
      name: 'Primary OpenAI',
      endpoint: 'https://api.openai.com/v1/',
      apiVersion: 'ignored',
      cloudRegion: 'ignored',
      cloudProject: 'ignored',
      deployment: 'ignored',
      authMode: 'api_key',
      options: undefined,
      profileId: '',
      profileRevision: ''
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
    connector_ready: true,
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
    expect(
      activationReady({ ...readyDraft, certified_capability_count: 1 })
    ).toBe(false);
    expect(
      activationReady({ ...readyDraft, last_probe_at: '2026-07-12T11:59:00Z' })
    ).toBe(false);
    expect(activationReady({ ...readyDraft, state: 'active' })).toBe(false);
    expect(
      activationReady({
        ...readyDraft,
        state: 'active',
        pending_activation: true
      })
    ).toBe(true);
    expect(
      activationReady({
        ...readyDraft,
        state: 'disabled',
        pending_activation: true
      })
    ).toBe(false);
    expect(activationReady({ ...readyDraft, state: 'disabled' })).toBe(false);
  });

  it('refuses activation when the build carries no connector for the kind', () => {
    // connector_ready is a server-owned build signal, not a capability count:
    // certified capabilities alone do not make the draft activatable.
    const withoutConnector = { ...readyDraft, connector_ready: false };
    expect(capabilitiesCertified(withoutConnector)).toBe(true);
    expect(activationReady(withoutConnector)).toBe(false);
  });

  it('requires fresh catalog evidence only for native provider certification', () => {
    const staleProbe = {
      ...readyDraft,
      configuration: { kind: 'openai' as const },
      last_probe_at: '2026-07-12T11:59:00Z'
    };
    expect(certificationPrerequisiteReady(staleProbe)).toBe(false);
    expect(
      certificationPrerequisiteReady({
        ...staleProbe,
        configuration: { kind: 'openai_compatible' }
      })
    ).toBe(true);
  });

  it('labels pending changes before active and draft states', () => {
    expect(
      providerStatus({
        ...readyDraft,
        active_revision: null,
        pending_activation: true
      })
    ).toBe('draft');
    expect(
      providerStatus({
        ...readyDraft,
        active_revision: 2,
        pending_activation: true
      })
    ).toBe('revision 2 live · changes pending');
    expect(
      providerStatus({
        ...readyDraft,
        active_revision: 2,
        pending_activation: false
      })
    ).toBe('revision 2 active');
    expect(
      providerStatus({
        ...readyDraft,
        active_revision: null,
        pending_activation: false
      })
    ).toBe('draft');
  });

  it('labels and tones a disabled provider', () => {
    const disabled = {
      ...readyDraft,
      state: 'disabled',
      active_revision: null,
      pending_activation: false
    };
    expect(providerStatus(disabled)).toBe('disabled · not serving');
    expect(providerStatusTone(disabled)).toBe('danger');
    expect(providerStatusTone({ ...disabled, state: 'draft' })).toBe('warning');
    expect(
      providerStatusTone({ ...disabled, state: 'active', active_revision: 3 })
    ).toBe('success');
    expect(
      providerStatusTone({
        ...disabled,
        state: 'active',
        active_revision: 3,
        pending_activation: true
      })
    ).toBe('warning');
  });

  it('keeps the disabled state ahead of an active revision the API still reports', () => {
    // A provider can come back disabled while `active_revision` still names the
    // revision that was serving. It serves nothing now, so the disabled state
    // wins in the status line, the badge tone, and the activation note.
    const disabledWithRevision = {
      ...readyDraft,
      state: 'disabled',
      active_revision: 4,
      pending_activation: false
    };
    expect(providerDisabled(disabledWithRevision)).toBe(true);
    expect(providerStatus(disabledWithRevision)).toBe('disabled · not serving');
    expect(providerStatusTone(disabledWithRevision)).toBe('danger');
    expect(
      providerStatus({ ...disabledWithRevision, pending_activation: true })
    ).toBe('disabled · not serving');
    expect(providerDisabled({ ...disabledWithRevision, state: 'active' })).toBe(
      false
    );
  });
});

describe('disableNotice', () => {
  it('names the runtime generation that published the disable', () => {
    expect(disableNotice(9)).toBe('Provider disabled in runtime generation 9.');
  });

  it('says nothing is serving when the disable published no generation', () => {
    expect(disableNotice(null)).toBe(
      'Provider disabled. No revision is serving traffic.'
    );
  });

  it('points a locked editor at the restore action', () => {
    expect(DISABLED_EDIT_NOTE).toContain('Restore it as a draft');
  });
});

describe('reviewed capability cap', () => {
  it('stops at the server limit of 64 tuples per model', () => {
    expect(MAX_REVIEWED_CAPABILITIES).toBe(64);
    expect(capabilityLimitReached(63)).toBe(false);
    expect(capabilityLimitReached(64)).toBe(true);
    expect(capabilityLimitReached(65)).toBe(true);
  });
});

describe('probeSummary', () => {
  it('names the probe that ran and the models it saw', () => {
    expect(
      probeSummary({
        probe_type: 'connector_connectivity',
        detail: 'OpenAI reachable',
        discovered_models: 12
      })
    ).toBe('OpenAI reachable · connector connectivity probe · 12 models seen');
  });

  it('omits the model count when the probe did not list models', () => {
    expect(
      probeSummary({
        probe_type: 'model_listing',
        detail: 'Endpoint reachable',
        discovered_models: null
      })
    ).toBe('Endpoint reachable · model listing probe');
  });

  it('keeps a single discovered model singular', () => {
    expect(
      probeSummary({
        probe_type: 'model_listing',
        detail: 'Endpoint reachable',
        discovered_models: 1
      })
    ).toBe('Endpoint reachable · model listing probe · 1 model seen');
  });
});

describe('plugin providers', () => {
  const digest = 'c'.repeat(64);
  const profile = (model_discovery?: boolean) =>
    referenceProfile(digest, { model_discovery });

  it('pins a plugin profile by its digest and leaves the address to the server', () => {
    const draft = createProviderDraft(pluginSpec);
    draft.endpoint = 'https://previous.example/v1';
    selectPluginProfile(draft, {
      id: 'reference-chat',
      revision: digest,
      authentication: ['static_credential']
    });
    draft.name = 'Reference';
    draft.model = 'reference-model';
    draft.credential = 'secret';
    const input = buildCreateProviderInput(draft, pluginSpec);
    expect(input.configuration).toMatchObject({
      kind: 'plugin',
      auth_mode: 'static_credential',
      profile_id: 'reference-chat',
      profile_revision: digest
    });
    expect(input.configuration.endpoint ?? null).toBeNull();
    expect(input.credential).toBe('secret');
    expect(input.model).toBe('reference-model');

    selectPluginProfile(draft, undefined);
    expect(draft.profileId).toBe('');
    expect(draft.document?.at(['profile_id'])).toBeUndefined();
  });

  it('requires a plugin profile, and a probe model unless the profile discovers models', () => {
    const draft = createProviderDraft(pluginSpec);
    draft.name = 'Reference';
    draft.credential = 'secret';
    expect(requiresProbeModel(draft, pluginSpec)).toBe(true);
    expect(
      requiresProbeModel({ kind: 'openai', presetId: '' }, openAiSpec)
    ).toBe(false);
    expect(validateProviderDraft(draft, pluginSpec)).toBe(
      'Provider plugin requires plugin profile, probe model.'
    );
    selectPluginProfile(draft, {
      id: 'reference-chat',
      revision: digest,
      authentication: ['static_credential']
    });
    // Pinning a profile or build drops a credential typed for a previous pin.
    expect(draft.credential).toBe('');
    draft.credential = 'secret';
    // Until the catalogue says the profile discovers models, it declares them.
    expect(requiresProbeModel(draft, pluginSpec)).toBe(true);
    expect(requiresProbeModel(draft, pluginSpec, [profile()])).toBe(true);
    expect(
      validateProviderDraft(draft, pluginSpec, { profiles: [profile()] })
    ).toBe('Provider plugin requires probe model.');
    expect(requiresProbeModel(draft, pluginSpec, [profile(true)])).toBe(false);
    expect(
      validateProviderDraft(draft, pluginSpec, { profiles: [profile(true)] })
    ).toBeNull();
    draft.model = 'reference-model';
    expect(validateProviderDraft(draft, pluginSpec)).toBeNull();
  });

  it('takes the authentication the profile declares, and no pasted credential for a grant', () => {
    const draft = createProviderDraft(pluginSpec);
    draft.name = 'Reference account';
    draft.model = 'reference-model';
    selectPluginProfile(draft, {
      id: 'reference-grant-chat',
      revision: digest,
      authentication: ['grant']
    });
    expect(draft.authMode).toBe('grant');
    expect(requiresGrant(pluginSpec, draft.authMode)).toBe(true);
    expect(requiresCredential(pluginSpec, draft.authMode)).toBe(false);
    expect(validateProviderDraft(draft, pluginSpec)).toBeNull();
    draft.credential = 'pasted';
    const input = buildCreateProviderInput(draft, pluginSpec);
    expect(input.configuration.auth_mode).toBe('grant');
    expect(input.credential).toBeUndefined();

    selectPluginProfile(draft, {
      id: 'reference-chat',
      revision: digest,
      authentication: ['static_credential']
    });
    expect(draft.authMode).toBe('static_credential');
    expect(requiresGrant(pluginSpec, draft.authMode)).toBe(false);
    // The pasted credential does not carry to the newly pinned profile.
    expect(draft.credential).toBe('');
  });
});
