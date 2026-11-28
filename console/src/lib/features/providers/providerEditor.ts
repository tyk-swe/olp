import { validated, type Validation } from '#lib/editing/validation.js';
import {
  ConfigurationDraft,
  type ConfigurationPatch
} from './configurationDraft.svelte';
import { ProviderCreateDraft, ProviderEditDraft } from './providerDraft.svelte';
import { parseExternalReference } from './externalReference';
import { nativeObject } from '#lib/nativeJson.js';
import type {
  Provider,
  ProviderKind,
  ProviderKindCapability
} from '#lib/domain/providers/api.js';
import type {
  CreateProviderInput,
  ProviderAuthMode,
  UpdateProviderInput
} from '#lib/features/providers/providers.js';
import type { ProviderPreset } from '#lib/features/providers/models.js';
import { pluginOptionFields } from './profiles';
import type { ProviderProfile } from './providerProfiles';
import {
  hasApiVersion,
  hasCloudProject,
  hasCloudRegion,
  hasCustomEndpoint,
  hasDeployment,
  requiresCredential
} from './providerKindSpec';

export type ProviderEditValues = {
  /** The sole configuration authority; the remaining properties are form views. */
  document?: ConfigurationDraft;
  profileId?: string;
  profileRevision?: string;
  options?: Provider['configuration']['options'];
  name: string;
  endpoint: string;
  apiVersion: string;
  cloudRegion: string;
  cloudProject: string;
  deployment: string;
  authMode: ProviderAuthMode;
};

export type ProviderDraft = ProviderEditValues & {
  kind: ProviderKind;
  credential: string;
  useExternalCredential?: boolean;
  externalCredentialReference?: string;
  model: string;
  projectId: string;
  /** Console-only selection; creation persists the resolved ordinary fields. */
  presetId: string;
  credentialHeaders?: string;
};

/** Connection options for a provider with no vendor, limits, or model metadata. */
export function emptyProviderOptions(): NonNullable<
  Provider['configuration']['options']
> {
  return {
    vendor_id: null,
    limits: null,
    credential_headers: [],
    parameter_defaults: {},
    models: {},
    shared_token_quotas: []
  };
}

export function createProviderDraft(
  spec: ProviderKindCapability,
  configuration?: Provider['configuration']
): ProviderDraft {
  return new ProviderCreateDraft(
    configuration ?? {
      kind: spec.kind,
      auth_mode: spec.default_auth_mode,
      endpoint: null,
      api_version: null,
      cloud_region: null,
      cloud_project: null,
      deployment: null,
      options: emptyProviderOptions()
    }
  );
}

export function setProviderDraftKind(
  draft: ProviderDraft,
  kind: ProviderKind
): void {
  if (draft.kind === kind) return;
  // Connector fields are kind-specific. Carrying an Azure resource endpoint or
  // a preset-resolved URL into the next connector silently persists a value the
  // operator never chose for it, and a secret typed for one upstream must never
  // be submitted as another upstream's credential.
  if (draft.document) {
    const hadProfile = Boolean(draft.profileId);
    draft.document.patch([
      [['kind'], kind],
      [['profile_id'], undefined],
      ...(hadProfile
        ? ([[['batch_storage'], null]] as ConfigurationPatch[])
        : []),
      [['profile_revision'], undefined],
      [['options'], undefined],
      [['endpoint'], null],
      [['api_version'], null],
      [['cloud_region'], null],
      [['cloud_project'], null],
      [['deployment'], null]
    ]);
  } else {
    draft.kind = kind;
    draft.profileId = '';
    draft.profileRevision = '';
    draft.presetId = '';
    draft.options = undefined;
    draft.credentialHeaders = '';
    draft.endpoint = '';
    draft.apiVersion = '';
    draft.cloudRegion = '';
    draft.cloudProject = '';
    draft.deployment = '';
  }
  draft.credential = '';
  draft.useExternalCredential = false;
  draft.externalCredentialReference = '';
  draft.model = '';
}

export function selectProviderPreset(
  draft: ProviderDraft,
  spec: ProviderKindCapability,
  presetId: string
): ProviderPreset | null {
  const previous = spec.presets.find(
    (candidate) => candidate.id === draft.presetId
  );
  const preset = presetId
    ? spec.presets.find((candidate) => candidate.id === presetId)
    : undefined;
  if (presetId && !preset)
    throw new Error('The selected provider preset is unavailable.');

  const changedPreset = draft.presetId !== presetId;
  // A preset's profile follows the preset only while the operator has not
  // chosen another: an untouched profile is replaced, a chosen one is kept.
  const followPresetProfile =
    draft.profileId === (previous?.profile_id ?? '') &&
    draft.profileRevision === (previous?.profile_revision ?? '');
  const nextProfileId = preset?.profile_id ?? '';
  const nextProfileRevision = preset?.profile_revision ?? '';

  if (draft.document) {
    const updates: ConfigurationPatch[] = [
      ...(changedPreset
        ? ([[['options'], undefined]] as ConfigurationPatch[])
        : []),
      ...(followPresetProfile
        ? ([
            [['profile_id'], nextProfileId || undefined],
            ...(draft.profileId !== nextProfileId
              ? ([[['batch_storage'], null]] as ConfigurationPatch[])
              : []),
            [['profile_revision'], nextProfileRevision || undefined]
          ] as ConfigurationPatch[])
        : []),
      ...(preset
        ? ([
            [['options', 'vendor_id'], preset.id],
            [['endpoint'], preset.endpoint.trim() || null],
            [['auth_mode'], preset.auth_mode]
          ] as ConfigurationPatch[])
        : ([
            [['options', 'vendor_id'], null],
            [['endpoint'], null],
            [['auth_mode'], spec.default_auth_mode]
          ] as ConfigurationPatch[]))
    ];
    draft.document.patch(updates);
  } else {
    if (changedPreset) {
      draft.options = undefined;
      draft.credentialHeaders = '';
    }
    if (followPresetProfile) {
      draft.profileId = nextProfileId;
      draft.profileRevision = nextProfileRevision;
    }
    draft.presetId = preset?.id ?? '';
    draft.endpoint = preset?.endpoint ?? '';
    draft.authMode = preset?.auth_mode ?? spec.default_auth_mode;
  }

  if (changedPreset) {
    draft.credential = '';
    draft.useExternalCredential = false;
    draft.externalCredentialReference = '';
  }
  return preset ?? null;
}

/**
 * Pins a plugin profile: its identity and the digest of the plugin build that
 * supplies it, and the authentication it declares, a static credential or a
 * grant. A plugin provider's endpoint is that profile's address, which the
 * server sets, so any address of a previous pin is cleared, and a credential
 * typed for one profile or build is never submitted as another's. Option
 * values carry over to the options the new profile also declares.
 */
export function selectPluginProfile(
  values: ProviderEditValues & { credential?: string },
  profile:
    | Pick<
        ProviderProfile,
        'id' | 'revision' | 'options_schema' | 'authentication'
      >
    | undefined
): void {
  const nextProfileId = profile?.id ?? '';
  const nextProfileRevision = profile?.revision ?? '';
  if (
    values.profileId !== nextProfileId ||
    values.profileRevision !== nextProfileRevision
  ) {
    values.credential = '';
  }
  const authentication = profile?.authentication[0];
  if (values.document) {
    const previousProfileId = values.document.text(['profile_id']);
    const updates: ConfigurationPatch[] = [
      [['profile_id'], nextProfileId || undefined],
      ...(previousProfileId !== nextProfileId
        ? ([[['batch_storage'], null]] as ConfigurationPatch[])
        : []),
      [['profile_revision'], nextProfileRevision || undefined],
      [['endpoint'], null],
      ...(authentication
        ? ([[['auth_mode'], authentication]] as ConfigurationPatch[])
        : [])
    ];
    const options = values.document.at(['options', 'plugin_options']);
    if (nativeObject(options)) {
      const declared = pluginOptionFields(profile).map((option) => option.name);
      const kept = Object.entries(options).filter(([name]) =>
        declared.includes(name)
      );
      updates.push([
        ['options', 'plugin_options'],
        kept.length ? Object.fromEntries(kept) : undefined
      ]);
    }
    values.document.patch(updates);
    return;
  }
  values.profileId = nextProfileId;
  values.profileRevision = nextProfileRevision;
  values.endpoint = '';
  if (authentication) values.authMode = authentication as ProviderAuthMode;
}

function buildConnectionFields(
  values: ProviderEditValues,
  spec: ProviderKindCapability
): Omit<Provider['configuration'], 'kind' | 'options'> {
  return {
    ...(values.profileId
      ? {
          profile_id: values.profileId,
          profile_revision: values.profileRevision
        }
      : {}),
    auth_mode: values.authMode,
    endpoint: hasCustomEndpoint(spec) ? values.endpoint.trim() || null : null,
    api_version: hasApiVersion(spec) ? values.apiVersion.trim() || null : null,
    cloud_region: hasCloudRegion(spec)
      ? values.cloudRegion.trim() || null
      : null,
    cloud_project: hasCloudProject(spec)
      ? values.cloudProject.trim() || null
      : null,
    batch_storage: null,
    deployment: hasDeployment(spec) ? values.deployment.trim() || null : null
  };
}

export function buildCreateProviderInput(
  draft: ProviderDraft,
  spec: ProviderKindCapability
): CreateProviderInput {
  return {
    name: draft.name.trim(),
    project_id: draft.projectId || null,
    configuration: draft.document
      ? draft.document.configuration()
      : {
          kind: draft.kind,
          options: {
            ...emptyProviderOptions(),
            ...draft.options,
            vendor_id: draft.presetId || draft.options?.vendor_id || null,
            credential_headers:
              draft.credentialHeaders
                ?.split(/[\n,]/)
                .map((s) => s.trim())
                .filter(Boolean) ?? []
          },
          ...buildConnectionFields(draft, spec)
        },
    ...(draft.useExternalCredential && requiresCredential(spec, draft.authMode)
      ? {
          credential_reference: parseExternalReference(
            draft.externalCredentialReference ?? ''
          )
        }
      : {}),
    credential:
      requiresCredential(spec, draft.authMode) && !draft.useExternalCredential
        ? draft.credential
        : undefined,
    model: draft.model.trim() || undefined,
    display_name: draft.model.trim() ? draft.name.trim() : undefined
  };
}

export function providerEditValues(
  current: Pick<Provider, 'name' | 'configuration'>,
  _spec?: ProviderKindCapability
): ProviderEditValues {
  void _spec;
  return new ProviderEditDraft(
    current.name,
    current.configuration,
    current.configuration.kind
  );
}

export function buildUpdateProviderInput(
  values: ProviderEditValues,
  spec: ProviderKindCapability
): UpdateProviderInput {
  return {
    name: values.name.trim(),
    configuration: values.document
      ? values.document.configuration()
      : {
          kind: spec.kind,
          options: values.options,
          ...buildConnectionFields(values, spec)
        }
  };
}

export function parseManualModelNames(value: string): string[] {
  return value
    .split(/[\n,]/)
    .map((model) => model.trim())
    .filter(Boolean);
}

export function validateModelNames(
  value: string
): Validation<string[], 'manualModelNames'> {
  const names = parseManualModelNames(value);
  return validated(
    {
      manualModelNames: names.length
        ? undefined
        : 'Enter at least one upstream model identifier.'
    },
    () => names
  );
}
