import { describe, expect, it } from 'vitest';
import type { ProviderKindCapability, ProviderPreset } from './models';
import {
  activationReady,
  capabilitiesCertified,
  capabilityLimitReached,
  certificationPrerequisiteReady,
  disableNotice,
  MAX_REVIEWED_CAPABILITIES,
  parseManualModelNames,
  probeReady,
  probeSummary,
  providerStatus,
  providerStatusTone,
  type ProviderReadiness,
  buildCreateProviderInput,
  buildUpdateProviderInput,
  createProviderDraft,
  providerEditValues,
  requiresProbeModel,
  selectPluginProfile,
  selectProviderPreset,
  setProviderDraftKind,
  validateProviderDraft
} from './providerEditor';

const preset: ProviderPreset = {
  id: 'hosted',
  label: 'Hosted',
  description: '',
  documentation_label: 'Documentation',
  documentation_url: 'https://example.test/docs',
  endpoint: 'https://example.test/v1',
  auth_mode: 'api_key',
  discovery: true,
  maintainer: 'Maintainer',
  placeholder: false,
  profile_id: 'hosted-profile',
  profile_revision: 'v1'
};

const spec: ProviderKindCapability = {
  kind: 'openai_compatible',
  label: 'Compatible provider',
  description: '',
  default_auth_mode: 'api_key',
  auth_modes: [
    { mode: 'api_key', label: 'API key', credential: 'required' },
    { mode: 'none', label: 'None', credential: 'forbidden' }
  ],
  fields: [{ field: 'endpoint', label: 'Endpoint', required: true }],
  presets: [preset]
};

const emptyOptions = {
  vendor_id: null,
  limits: null,
  credential_headers: [],
  parameter_defaults: {},
  models: {}
};

describe('provider editing', () => {
  it('requires the name, endpoint, and credential before creating a provider', () => {
    const draft = createProviderDraft(spec);
    expect(validateProviderDraft(draft, spec)).toBe(
      'Compatible provider requires name, endpoint, credential.'
    );

    draft.name = '  Service  ';
    draft.endpoint = '  https://example.test/v1  ';
    draft.credential = 'secret';
    draft.model = '  model-id  ';
    draft.credentialHeaders = ' X-Token, X-Tenant\nX-Other ';

    expect(validateProviderDraft(draft, spec)).toBeNull();
    expect(buildCreateProviderInput(draft, spec)).toMatchObject({
      name: 'Service',
      project_id: null,
      credential: 'secret',
      model: 'model-id',
      display_name: 'Service',
      configuration: {
        kind: 'openai_compatible',
        endpoint: 'https://example.test/v1',
        auth_mode: 'api_key',
        options: { credential_headers: ['X-Token', 'X-Tenant', 'X-Other'] }
      }
    });
  });

  it('sends an external credential reference instead of a pasted secret', () => {
    const draft = createProviderDraft(spec);
    draft.name = 'Service';
    draft.endpoint = 'https://example.test/v1';
    draft.useExternalCredential = true;
    draft.credential = 'secret from an earlier selection';
    expect(validateProviderDraft(draft, spec)).toContain('credential');

    const reference = {
      store: 'vault',
      secret_id: 'https://vault.example.test/v1/secret/data/service',
      version: '1',
      field: 'api_key'
    };
    draft.externalCredentialReference = JSON.stringify(reference);

    expect(validateProviderDraft(draft, spec)).toBeNull();
    expect(buildCreateProviderInput(draft, spec)).toMatchObject({
      credential: undefined,
      credential_reference: reference
    });
  });

  it.each([
    {
      store: 'aws',
      secret_id: 'prod/provider',
      version: 'a1b2c3d4-e5f6-7890-abcd-ef1234567890',
      region: 'us-east-1'
    },
    {
      store: 'gcp',
      secret_id: 'projects/project/secrets/provider',
      version: '3'
    },
    {
      store: 'azure',
      secret_id: 'https://vault.vault.azure.net/secrets/provider',
      version: '0123456789abcdef0123456789abcdef'
    },
    {
      store: 'vault',
      secret_id: 'HTTPS://Vault.example.test/v1/secret/data/service',
      version: '1',
      field: 'api_key'
    }
  ])('accepts a version-pinned $store reference', (reference) => {
    const draft = createProviderDraft(spec);
    draft.name = 'Service';
    draft.endpoint = 'https://example.test/v1';
    draft.useExternalCredential = true;
    draft.externalCredentialReference = JSON.stringify(reference);

    expect(validateProviderDraft(draft, spec)).toBeNull();
  });

  it.each([
    [
      'not JSON',
      'Enter a JSON object with store, secret_id and immutable version.'
    ],
    [
      '{"store":"vault","secret_id":"service-key","version":""}',
      'Choose a supported store, secret ID and immutable version.'
    ],
    [
      '{"store":"vault","secret_id":"service-key","version":"1"}',
      'Vault KV v2 references require a version and field.'
    ],
    [
      '{"store":"vault","secret_id":"https://vault.example.test/v1/secret/data/service","version":"1"}',
      'Vault KV v2 references require a version and field.'
    ],
    [
      '{"store":"vault","secret_id":"https://@vault.example.test/v1/secret/data/service","version":"1","field":"api_key"}',
      'Vault KV v2 references require a version and field.'
    ],
    [
      JSON.stringify({
        store: 'vault',
        secret_id: String.raw`https:\\vault.example.test\v1\secret\data\service`,
        version: '1',
        field: 'api_key'
      }),
      'Vault KV v2 references require a version and field.'
    ],
    [
      '{"store":"azure","secret_id":"https://VAULT.VAULT.AZURE.NET/secrets/name","version":"0123456789abcdef0123456789abcdef"}',
      'Azure references require a versioned vault secret.'
    ],
    [
      '{"store":"aws","secret_id":"prod/provider","version":"latest","region":"us-east-1"}',
      'AWS references require a region and immutable version ID.'
    ],
    [
      '{"store":"aws","secret_id":"prod/provider","version":"AWSCURRENT","region":"us-east-1"}',
      'AWS references require a region and immutable version ID.'
    ],
    [
      '{"store":"gcp","secret_id":"projects/project/secrets/provider","version":"latest"}',
      'Google references require a project secret and numeric version.'
    ],
    [
      '{"store":"azure","secret_id":"https://vault.vault.azure.net/secrets/name","version":"latest"}',
      'Azure references require a versioned vault secret.'
    ]
  ])(
    'rejects an invalid external credential reference before submission',
    (reference, issue) => {
      const draft = createProviderDraft(spec);
      draft.name = 'Service';
      draft.endpoint = 'https://example.test/v1';
      draft.useExternalCredential = true;
      draft.externalCredentialReference = reference;

      expect(validateProviderDraft(draft, spec)).toBe(issue);
    }
  );

  it('clears connection details and credentials when switching kind', () => {
    const draft = createProviderDraft(spec);
    draft.name = 'Service';
    draft.endpoint = 'https://example.test/v1';
    draft.profileId = 'profile';
    draft.profileRevision = 'v1';
    draft.credential = 'secret';
    draft.useExternalCredential = true;
    draft.externalCredentialReference = '{"store":"vault"}';
    draft.credentialHeaders = 'X-Token';
    draft.model = 'old-model';

    setProviderDraftKind(draft, 'anthropic');

    expect(draft).toMatchObject({
      kind: 'anthropic',
      name: 'Service',
      endpoint: '',
      profileId: '',
      profileRevision: '',
      credential: '',
      useExternalCredential: false,
      externalCredentialReference: '',
      credentialHeaders: '',
      model: ''
    });
  });

  it('clears credentials on preset changes but preserves a manually chosen profile', () => {
    const draft = createProviderDraft(spec);
    expect(selectProviderPreset(draft, spec, preset.id)).toEqual(preset);
    expect(draft).toMatchObject({
      presetId: preset.id,
      endpoint: preset.endpoint,
      profileId: preset.profile_id,
      profileRevision: preset.profile_revision
    });
    expect(requiresProbeModel(draft, spec)).toBe(false);

    draft.profileId = 'custom-profile';
    draft.profileRevision = 'custom-revision';
    draft.credential = 'secret';
    draft.credentialHeaders = 'X-Token';
    expect(selectProviderPreset(draft, spec, '')).toBeNull();
    expect(draft).toMatchObject({
      presetId: '',
      endpoint: '',
      profileId: 'custom-profile',
      profileRevision: 'custom-revision',
      credential: '',
      credentialHeaders: ''
    });
  });

  it('keeps unrelated configuration when editing a saved provider', () => {
    const values = providerEditValues(
      {
        name: 'Service',
        configuration: {
          kind: 'openai_compatible',
          auth_mode: 'api_key',
          endpoint: 'https://old.example.test',
          options: {
            ...emptyOptions,
            vendor_id: 'hosted',
            parameter_defaults: { response_format: 'json' }
          }
        }
      },
      spec
    );
    values.name = '  Renamed  ';
    values.endpoint = '  https://new.example.test/v1  ';

    expect(buildUpdateProviderInput(values, spec)).toMatchObject({
      name: 'Renamed',
      configuration: {
        kind: 'openai_compatible',
        endpoint: 'https://new.example.test/v1',
        options: {
          vendor_id: 'hosted',
          parameter_defaults: { response_format: 'json' }
        }
      }
    });
  });

  it('clears stale credentials and undeclared options when selecting a plugin profile', () => {
    const draft = createProviderDraft(
      { ...spec, kind: 'plugin' },
      {
        kind: 'plugin',
        auth_mode: 'static_credential',
        endpoint: 'https://old.example.test',
        profile_id: 'old-profile',
        profile_revision: 'old-revision',
        options: {
          ...emptyOptions,
          plugin_options: { keep: 'value', remove: 'stale' }
        }
      }
    );
    draft.credential = 'secret';

    selectPluginProfile(draft, {
      id: 'new-profile',
      revision: 'new-revision',
      authentication: ['grant'],
      options_schema: { properties: { keep: { type: 'string' } } }
    });

    expect(draft).toMatchObject({
      profileId: 'new-profile',
      profileRevision: 'new-revision',
      endpoint: '',
      authMode: 'grant',
      credential: ''
    });
    expect(draft.document?.configuration().options?.plugin_options).toEqual({
      keep: 'value'
    });
  });
});

const ready: ProviderReadiness = {
  state: 'draft',
  connector_ready: true,
  enabled_model_count: 1,
  capability_count: 2,
  certified_capability_count: 2,
  last_probe_at: '2026-01-02T00:00:00Z',
  last_probe_status: 'succeeded',
  updated_at: '2026-01-01T00:00:00Z'
};

describe('provider activation readiness', () => {
  it('requires a successful probe no older than the configuration', () => {
    expect(probeReady(ready)).toBe(true);
    expect(probeReady({ ...ready, last_probe_at: ready.updated_at })).toBe(
      true
    );
    expect(probeReady({ ...ready, last_probe_at: null })).toBe(false);
    expect(probeReady({ ...ready, last_probe_status: 'failed' })).toBe(false);
    expect(
      probeReady({ ...ready, last_probe_at: '2025-12-31T00:00:00Z' })
    ).toBe(false);
    expect(probeReady(null)).toBe(false);
  });

  it('allows compatible providers to certify without a probe but requires it for other kinds', () => {
    const unprobed = { ...ready, last_probe_at: null };
    expect(
      certificationPrerequisiteReady({
        ...unprobed,
        configuration: { kind: 'openai_compatible' }
      })
    ).toBe(true);
    expect(
      certificationPrerequisiteReady({
        ...unprobed,
        configuration: { kind: 'anthropic' }
      })
    ).toBe(false);
    expect(
      certificationPrerequisiteReady({
        ...ready,
        configuration: { kind: 'anthropic' }
      })
    ).toBe(true);
    expect(certificationPrerequisiteReady(null)).toBe(false);
  });

  it('requires enabled models and every capability to be certified', () => {
    expect(capabilitiesCertified(ready)).toBe(true);
    expect(capabilitiesCertified({ ...ready, enabled_model_count: 0 })).toBe(
      false
    );
    expect(
      capabilitiesCertified({
        ...ready,
        capability_count: 0,
        certified_capability_count: 0
      })
    ).toBe(false);
    expect(
      capabilitiesCertified({ ...ready, certified_capability_count: 1 })
    ).toBe(false);
    expect(capabilitiesCertified(null)).toBe(false);
  });

  it('permits activation only for a ready draft or active provider with pending changes', () => {
    expect(activationReady(ready)).toBe(true);
    expect(
      activationReady({ ...ready, state: 'active', pending_activation: true })
    ).toBe(true);
    expect(
      activationReady({ ...ready, state: 'active', pending_activation: false })
    ).toBe(false);
    expect(activationReady({ ...ready, state: 'disabled' })).toBe(false);
    expect(activationReady({ ...ready, connector_ready: false })).toBe(false);
    expect(activationReady({ ...ready, certified_capability_count: 1 })).toBe(
      false
    );
    expect(activationReady({ ...ready, last_probe_status: 'failed' })).toBe(
      false
    );
    expect(activationReady(null)).toBe(false);
  });

  it('describes disabled, active and pending revisions with matching tones', () => {
    expect(
      providerStatus({
        state: 'disabled',
        active_revision: 2,
        pending_activation: true
      })
    ).toBe('disabled · not serving');
    expect(
      providerStatusTone({
        state: 'disabled',
        active_revision: 2,
        pending_activation: true
      })
    ).toBe('danger');
    expect(
      providerStatus({
        state: 'active',
        active_revision: 2,
        pending_activation: true
      })
    ).toBe('revision 2 live · changes pending');
    expect(
      providerStatusTone({
        state: 'active',
        active_revision: 2,
        pending_activation: true
      })
    ).toBe('warning');
    expect(
      providerStatus({
        state: 'active',
        active_revision: 2,
        pending_activation: false
      })
    ).toBe('revision 2 active');
    expect(
      providerStatusTone({
        state: 'active',
        active_revision: 2,
        pending_activation: false
      })
    ).toBe('success');
    expect(providerStatus({ state: 'draft', pending_activation: false })).toBe(
      'draft'
    );
    expect(
      providerStatusTone({ state: 'draft', pending_activation: false })
    ).toBe('warning');
  });

  it('reports disable generations, probe details and the review limit', () => {
    expect(disableNotice(null)).toContain('No revision is serving traffic');
    expect(disableNotice(7)).toContain('generation 7');
    expect(
      probeSummary({
        detail: 'Connected',
        probe_type: 'discovery',
        discovered_models: 1
      })
    ).toBe('Connected · discovery probe · 1 model seen');
    expect(
      probeSummary({
        detail: 'Connected',
        probe_type: 'discovery',
        discovered_models: 2
      })
    ).toBe('Connected · discovery probe · 2 models seen');
    expect(
      probeSummary({
        detail: 'Connected',
        probe_type: 'discovery',
        discovered_models: null
      })
    ).toBe('Connected · discovery probe');
    expect(capabilityLimitReached(MAX_REVIEWED_CAPABILITIES - 1)).toBe(false);
    expect(capabilityLimitReached(MAX_REVIEWED_CAPABILITIES)).toBe(true);
    expect(parseManualModelNames(' model-a, model-b\n\n model-c ')).toEqual([
      'model-a',
      'model-b',
      'model-c'
    ]);
  });
});
