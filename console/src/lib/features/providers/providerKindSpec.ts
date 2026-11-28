import type {
  ProviderKindCapability,
  ProviderKind,
  ProviderVendor
} from '#lib/domain/providers/api.js';
import type { ProviderAuthMode } from './providers';
import { declaresModels } from './profiles';
import type { ProviderProfile } from './providerProfiles';
import type { ProviderDraft, ProviderEditValues } from './providerEditor';

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

export function hasCustomEndpoint(spec: ProviderKindCapability): boolean {
  return hasField(spec, 'endpoint');
}

export function showsEndpoint(
  values: Pick<ProviderEditValues, 'profileId'>,
  spec: ProviderKindCapability
): boolean {
  return (
    spec.kind !== 'plugin' &&
    (hasCustomEndpoint(spec) || Boolean(values.profileId))
  );
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
