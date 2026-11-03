import { describe, expect, it } from 'vitest';
import type { ProviderKindCapability, ProviderPreset } from './api/models';
import {
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

    draft.externalCredentialReference = JSON.stringify({
      store: 'vault',
      secret_id: 'service-key',
      version: '1'
    });

    expect(validateProviderDraft(draft, spec)).toBeNull();
    expect(buildCreateProviderInput(draft, spec)).toMatchObject({
      credential: undefined,
      credential_reference: {
        store: 'vault',
        secret_id: 'service-key',
        version: '1'
      }
    });
  });

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
