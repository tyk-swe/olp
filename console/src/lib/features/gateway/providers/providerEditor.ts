import type {
  CreateProviderInput,
  ProviderAuthMode,
  ProviderKind,
  ProviderProbe,
  UpdateProviderInput
} from '$lib/api/management/providers';
import type {
  ProviderKindCapability,
  ProviderPreset
} from '$lib/api/management/providerModels';
import { stateLabel } from '$lib/format';

export type ProviderDraft = {
  kind: ProviderKind;
  name: string;
  endpoint: string;
  apiVersion: string;
  cloudRegion: string;
  cloudProject: string;
  deployment: string;
  authMode: ProviderAuthMode;
  credential: string;
  model: string;
  /** Console-only selection; creation persists the resolved ordinary fields. */
  presetId: string;
};

export type ProviderEditValues = {
  name: string;
  endpoint: string;
  apiVersion: string;
  cloudRegion: string;
  cloudProject: string;
  deployment: string;
  authMode: ProviderAuthMode;
};

export type ProviderReadiness = {
  state: string;
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

export function createProviderDraft(
  spec: ProviderKindCapability
): ProviderDraft {
  return {
    kind: spec.kind,
    name: '',
    endpoint: '',
    apiVersion: '',
    cloudRegion: '',
    cloudProject: '',
    deployment: '',
    authMode: spec.default_auth_mode,
    credential: '',
    model: '',
    presetId: ''
  };
}

export function setProviderDraftKind(
  draft: ProviderDraft,
  kind: ProviderKind
): void {
  if (draft.kind === kind) return;
  draft.kind = kind;
  // Connector fields are kind-specific. Carrying an Azure resource endpoint or
  // a preset-resolved URL into the next connector silently persists a value the
  // operator never chose for it, and a secret typed for one upstream must never
  // be submitted as another upstream's credential.
  draft.presetId = '';
  draft.credential = '';
  draft.endpoint = '';
  draft.apiVersion = '';
  draft.cloudRegion = '';
  draft.cloudProject = '';
  draft.deployment = '';
  draft.model = '';
}

export function selectProviderPreset(
  draft: ProviderDraft,
  spec: ProviderKindCapability,
  presetId: string
): ProviderPreset | null {
  if (!presetId) {
    draft.presetId = '';
    draft.endpoint = '';
    draft.authMode = spec.default_auth_mode;
    return null;
  }
  const preset = spec.presets.find((candidate) => candidate.id === presetId);
  if (!preset) throw new Error('The selected provider preset is unavailable.');
  draft.presetId = preset.id;
  draft.endpoint = preset.endpoint;
  draft.authMode = preset.auth_mode;
  return preset;
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
  options: { credentialAlreadyStored?: boolean } = {}
): string | null {
  const values: Record<string, string> = {
    endpoint: draft.endpoint,
    api_version: draft.apiVersion,
    cloud_region: draft.cloudRegion,
    cloud_project: draft.cloudProject,
    deployment: draft.deployment,
    model: draft.model
  };
  const missing = spec.fields
    .filter((field) => field.required && !values[field.field]?.trim())
    .map((field) => field.label.toLowerCase());
  if (!draft.name.trim()) missing.unshift('name');
  if (
    !options.credentialAlreadyStored &&
    requiresCredential(spec, draft.authMode) &&
    !draft.credential.trim()
  ) {
    // Re-editing a saved draft keeps the stored write-only credential; only a
    // provider that has never been created must supply one here.
    missing.push('credential');
  }
  if (!missing.length) return null;
  return `${spec.label} requires ${missing.join(', ')}.`;
}

export function buildCreateProviderInput(
  draft: ProviderDraft,
  spec: ProviderKindCapability
): CreateProviderInput {
  return {
    name: draft.name.trim(),
    kind: draft.kind,
    credential: draft.credential || undefined,
    model: draft.model.trim() || null,
    endpoint: hasCustomEndpoint(spec) ? draft.endpoint.trim() || null : null,
    api_version: hasApiVersion(spec) ? draft.apiVersion.trim() || null : null,
    cloud_region: hasCloudRegion(spec)
      ? draft.cloudRegion.trim() || null
      : null,
    cloud_project: hasCloudProject(spec)
      ? draft.cloudProject.trim() || null
      : null,
    deployment: hasDeployment(spec) ? draft.deployment.trim() || null : null,
    auth_mode: draft.authMode,
    display_name: draft.name.trim()
  };
}

export function providerEditValues(
  current: {
    name: string;
    kind: ProviderKind;
    endpoint?: string | null;
    api_version?: string | null;
    cloud_region?: string | null;
    cloud_project?: string | null;
    deployment?: string | null;
    auth_mode: ProviderAuthMode;
  },
  spec: ProviderKindCapability
): ProviderEditValues {
  return {
    name: current.name,
    endpoint: hasCustomEndpoint(spec) ? (current.endpoint ?? '') : '',
    apiVersion: hasApiVersion(spec) ? (current.api_version ?? '') : '',
    cloudRegion: hasCloudRegion(spec) ? (current.cloud_region ?? '') : '',
    cloudProject: hasCloudProject(spec) ? (current.cloud_project ?? '') : '',
    deployment: hasDeployment(spec) ? (current.deployment ?? '') : '',
    authMode: current.auth_mode
  };
}

export function buildUpdateProviderInput(
  values: ProviderEditValues,
  spec: ProviderKindCapability
): UpdateProviderInput {
  return {
    name: values.name.trim(),
    endpoint: hasCustomEndpoint(spec) ? values.endpoint.trim() || null : null,
    api_version: hasApiVersion(spec) ? values.apiVersion.trim() || null : null,
    cloud_region: hasCloudRegion(spec)
      ? values.cloudRegion.trim() || null
      : null,
    cloud_project: hasCloudProject(spec)
      ? values.cloudProject.trim() || null
      : null,
    deployment: hasDeployment(spec) ? values.deployment.trim() || null : null,
    auth_mode: values.authMode
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
  current: (ProviderReadiness & { kind: ProviderKind }) | null | undefined
): boolean {
  return current?.kind === 'openai_compatible' || probeReady(current);
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
    current?.state === 'draft' &&
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
  if (current.pending_activation)
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
