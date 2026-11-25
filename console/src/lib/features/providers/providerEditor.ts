import {
  ConfigurationDraft,
  type ConfigurationPatch
} from './configurationDraft.svelte';
import { ProviderCreateDraft, ProviderEditDraft } from './providerDraft.svelte';
import { parseExternalReference } from './externalReference';
import { nativeObject } from '#lib/nativeJson.js';
import type { Provider, ProviderVendor } from './providers';
import type {
  CreateProviderInput,
  ProviderAuthMode,
  ProviderKind,
  ProviderProbe,
  UpdateProviderInput
} from '#lib/features/providers/providers.js';
import type {
  ProviderKindCapability,
  ProviderPreset
} from '#lib/features/providers/models.js';
import { declaresModels, pluginOptionFields } from './profiles';
import type { ProviderProfile } from './providerProfiles';
import { stateLabel } from '#lib/format.js';

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

export type ProviderReadiness = {
  state: string;
  pending_activation?: boolean;
  connector_ready: boolean;
  enabled_model_count: number;
  capability_count: number;
  certified_capability_count: number;
  last_probe_at?: string | null;
  last_probe_status?: string | null;
  updated_at: string;
};

export type ProviderStatusValue = Pick<ProviderReadiness, 'state'> & {
  active_revision?: number | null;
  pending_activation: boolean;
};

/** Badge class for a provider status line. */
export type ProviderStatusTone = 'success' | 'warning' | 'danger';

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

export function authOptionsFor(
  spec: ProviderKindCapability
): readonly (readonly [ProviderAuthMode, string])[] {
  return spec.auth_modes.map((auth) => [auth.mode, auth.label] as const);
}

export function requiresCredential(
  spec: ProviderKindCapability,
  authMode: ProviderAuthMode
): boolean {
  return (
    spec.auth_modes.find((auth) => auth.mode === authMode)?.credential ===
    'required'
  );
}

/** Whether the provider authenticates with a grant, whose credential versions
 * come from grant enrollment rather than a pasted credential. */
export function requiresGrant(
  spec: ProviderKindCapability,
  authMode: ProviderAuthMode
): boolean {
  return (
    spec.auth_modes.find((auth) => auth.mode === authMode)?.credential ===
    'grant'
  );
}

function hasField(spec: ProviderKindCapability, field: string): boolean {
  return spec.fields.some((candidate) => candidate.field === field);
}

function requiresField(spec: ProviderKindCapability, field: string): boolean {
  return spec.fields.some(
    (candidate) => candidate.field === field && candidate.required
  );
}

export function requiresSeedModel(spec: ProviderKindCapability): boolean {
  return requiresField(spec, 'model');
}

/**
 * Whether creating the draft needs a probe model: the connection test
 * certifies a declared model when the upstream publishes no model list, as
 * for kinds that require one, presets and vendors whose upstream lists no
 * models, and plugin profiles that declare no model discovery.
 */
export function requiresProbeModel(
  draft: Pick<
    ProviderDraft,
    'kind' | 'presetId' | 'profileId' | 'profileRevision' | 'options'
  >,
  spec: ProviderKindCapability,
  profiles?: readonly ProviderProfile[],
  vendors?: readonly Pick<ProviderVendor, 'id' | 'discovery'>[]
): boolean {
  if (draft.kind === 'plugin')
    return declaresModels(
      {
        kind: draft.kind,
        profile_id: draft.profileId,
        profile_revision: draft.profileRevision
      },
      profiles
    );
  const preset = spec.presets.find(
    (candidate) => candidate.id === draft.presetId
  );
  const vendorId = draft.presetId || draft.options?.vendor_id;
  const vendor = vendors?.find((candidate) => candidate.id === vendorId);
  return (
    requiresSeedModel(spec) ||
    preset?.discovery === false ||
    vendor?.discovery === false
  );
}

/** How the connection form asks for the model a connection test probes. */
export function probeModelPrompt(kind: ProviderKind): {
  label: string;
  placeholder: string;
} {
  switch (kind) {
    case 'vertex_ai':
      return {
        label: 'Vertex probe model',
        placeholder: 'publishers/google/models/gemini-2.5-pro'
      };
    case 'sagemaker':
      return {
        label: 'SageMaker endpoint',
        placeholder: 'endpoint or endpoint/inference-component'
      };
    default:
      return { label: 'Probe model', placeholder: 'Exact upstream model ID' };
  }
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

export function hasCustomEndpoint(spec: ProviderKindCapability): boolean {
  return hasField(spec, 'endpoint');
}

export function hasCloudRegion(spec: ProviderKindCapability): boolean {
  return hasField(spec, 'cloud_region');
}

export function hasCloudProject(spec: ProviderKindCapability): boolean {
  return hasField(spec, 'cloud_project');
}

export function hasDeployment(spec: ProviderKindCapability): boolean {
  return hasField(spec, 'deployment');
}

export function hasApiVersion(spec: ProviderKindCapability): boolean {
  return hasField(spec, 'api_version');
}

export function validateProviderDraft(
  draft: ProviderDraft,
  spec: ProviderKindCapability,
  options: {
    credentialAlreadyStored?: boolean;
    /** The profile catalogue, which says whether a plugin profile discovers models. */
    profiles?: readonly ProviderProfile[];
    /** The vendor catalogue, which says whether a vendor lists its models. */
    vendors?: readonly ProviderVendor[];
  } = {}
): string | null {
  const values: Record<string, string> = {
    endpoint: draft.endpoint,
    api_version: draft.apiVersion,
    cloud_region: draft.cloudRegion,
    cloud_project: draft.cloudProject,
    deployment: draft.deployment,
    model: draft.model
  };
  if (draft.document?.issue) return draft.document.issue;
  // Versioned profiles own connection requirements; the server validates their
  // hosting adapter. Kind fields describe Automatic providers, not profiles.
  const profiled = Boolean(draft.profileId);
  const missing = spec.fields
    .filter(
      (field) =>
        field.required &&
        !(profiled && field.field !== 'model') &&
        !values[field.field]?.trim()
    )
    .map((field) => field.label.toLowerCase());
  if (draft.kind === 'plugin' && !draft.profileId)
    missing.unshift('plugin profile');
  if (!draft.name.trim()) missing.unshift('name');
  if (
    requiresProbeModel(draft, spec, options.profiles, options.vendors) &&
    !requiresSeedModel(spec) &&
    !draft.model.trim()
  )
    missing.push('probe model');
  if (
    !options.credentialAlreadyStored &&
    requiresCredential(spec, draft.authMode) &&
    !(draft.useExternalCredential
      ? draft.externalCredentialReference?.trim()
      : draft.credential.trim())
  ) {
    // Re-editing a saved draft keeps the stored write-only credential; only a
    // provider that has never been created must supply one here.
    missing.push('credential');
  }
  if (missing.length) return `${spec.label} requires ${missing.join(', ')}.`;
  if (draft.useExternalCredential && requiresCredential(spec, draft.authMode)) {
    try {
      parseExternalReference(draft.externalCredentialReference ?? '');
    } catch (error) {
      return error instanceof Error
        ? error.message
        : 'Enter a valid external credential reference.';
    }
  }
  return null;
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

/** The fields of a probe result this summary line reads. */
export type ProbeSummary = Pick<
  ProviderProbe,
  'probe_type' | 'detail' | 'discovered_models'
>;

/** Probe result line: the server detail, which probe ran, and models seen. */
export function probeSummary(probe: ProbeSummary): string {
  const parts = [probe.detail, `${stateLabel(probe.probe_type)} probe`];
  if (probe.discovered_models != null) {
    parts.push(
      `${probe.discovered_models} model${probe.discovered_models === 1 ? '' : 's'} seen`
    );
  }
  return parts.join(' · ');
}

export function probeReady(
  current: ProviderReadiness | null | undefined
): boolean {
  if (!current?.last_probe_at || current.last_probe_status !== 'succeeded')
    return false;
  return Date.parse(current.last_probe_at) >= Date.parse(current.updated_at);
}

export function certificationPrerequisiteReady(
  current:
    | (ProviderReadiness & { configuration: { kind: ProviderKind } })
    | null
    | undefined
): boolean {
  return (
    current?.configuration.kind === 'openai_compatible' || probeReady(current)
  );
}

export function capabilitiesCertified(
  current: ProviderReadiness | null | undefined
): boolean {
  if (!current) return false;
  return (
    current.enabled_model_count > 0 &&
    current.capability_count > 0 &&
    current.capability_count === current.certified_capability_count
  );
}

/**
 * Mirrors the server's activation precondition. `connector_ready` reports
 * whether this build carries a usable connector for the provider kind; it is
 * not a capability signal, so it gates activation alongside the certification
 * counts rather than replacing them.
 */
export function activationReady(
  current: ProviderReadiness | null | undefined
): boolean {
  return Boolean(
    (current?.state === 'draft' ||
      (current?.state === 'active' && current.pending_activation)) &&
    current.connector_ready &&
    capabilitiesCertified(current) &&
    probeReady(current)
  );
}

/**
 * A disabled provider serves nothing, so the disabled state outranks any
 * `active_revision` the API still reports on it. Both the status line and the
 * activation note read it in that order.
 */
export function providerDisabled(
  current: Pick<ProviderStatusValue, 'state'>
): boolean {
  return current.state === 'disabled';
}

/** Pointer shown wherever an edit control is locked by the disabled state. */
export const DISABLED_EDIT_NOTE =
  'This provider is disabled. Restore it as a draft to change configuration, rotate credentials, or review models again.';

/** Success line after a disable, naming the generation that published it. */
export function disableNotice(generation: number | null): string {
  return generation == null
    ? 'Provider disabled. No revision is serving traffic.'
    : `Provider disabled in runtime generation ${generation}.`;
}

export function providerStatus(current: ProviderStatusValue): string {
  if (providerDisabled(current)) return 'disabled · not serving';
  if (current.pending_activation && current.active_revision != null)
    return `revision ${current.active_revision} live · changes pending`;
  if (current.active_revision != null)
    return `revision ${current.active_revision} active`;
  return current.state;
}

export const MAX_REVIEWED_CAPABILITIES = 64;

export function capabilityLimitReached(reviewedCount: number): boolean {
  return reviewedCount >= MAX_REVIEWED_CAPABILITIES;
}

export function providerStatusTone(
  current: ProviderStatusValue
): ProviderStatusTone {
  if (providerDisabled(current)) return 'danger';
  if (current.pending_activation) return 'warning';
  return current.active_revision != null ? 'success' : 'warning';
}
