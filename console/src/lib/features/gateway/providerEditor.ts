import type {
  CreateProviderInput,
  ProviderAuthMode,
  ProviderKind,
  ProviderKindCapability,
  UpdateProviderInput
} from '$lib/api/management/providers';

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

export function createProviderDraft(spec: ProviderKindCapability): ProviderDraft {
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
    model: ''
  };
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
  return spec.auth_modes.find((auth) => auth.mode === authMode)?.credential === 'required';
}

function hasField(spec: ProviderKindCapability, field: string): boolean {
  return spec.fields.some((candidate) => candidate.field === field);
}

function requiresField(spec: ProviderKindCapability, field: string): boolean {
  return spec.fields.some((candidate) => candidate.field === field && candidate.required);
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
  spec: ProviderKindCapability
): string | null {
  if (
    !draft.name.trim() ||
    (requiresSeedModel(spec) && !draft.model.trim()) ||
    (requiresCredential(spec, draft.authMode) && !draft.credential)
  ) {
    return requiresSeedModel(spec)
      ? 'Name, Vertex probe model, and the selected identity fields are required.'
      : 'Name and the selected identity fields are required.';
  }
  const values: Record<string, string> = {
    endpoint: draft.endpoint,
    api_version: draft.apiVersion,
    cloud_region: draft.cloudRegion,
    cloud_project: draft.cloudProject,
    deployment: draft.deployment,
    model: draft.model
  };
  const missing = spec.fields.filter((field) => field.required && !values[field.field]?.trim());
  if (missing.length) {
    return `${spec.label} requires ${missing.map((field) => field.label.toLowerCase()).join(', ')}.`;
  }
  return null;
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
    cloud_region: hasCloudRegion(spec) ? draft.cloudRegion.trim() || null : null,
    cloud_project: hasCloudProject(spec) ? draft.cloudProject.trim() || null : null,
    deployment: hasDeployment(spec) ? draft.deployment.trim() || null : null,
    auth_mode: draft.authMode,
    display_name: draft.name.trim()
  };
}

export function providerEditValues(current: {
  name: string;
  kind: ProviderKind;
  endpoint?: string | null;
  api_version?: string | null;
  cloud_region?: string | null;
  cloud_project?: string | null;
  deployment?: string | null;
  auth_mode: ProviderAuthMode;
}, spec: ProviderKindCapability): ProviderEditValues {
  return {
    name: current.name,
    endpoint: hasCustomEndpoint(spec) ? current.endpoint ?? '' : '',
    apiVersion: hasApiVersion(spec) ? current.api_version ?? '' : '',
    cloudRegion: hasCloudRegion(spec) ? current.cloud_region ?? '' : '',
    cloudProject: hasCloudProject(spec) ? current.cloud_project ?? '' : '',
    deployment: hasDeployment(spec) ? current.deployment ?? '' : '',
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
    cloud_region: hasCloudRegion(spec) ? values.cloudRegion.trim() || null : null,
    cloud_project: hasCloudProject(spec) ? values.cloudProject.trim() || null : null,
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

export function probeReady(current: ProviderReadiness | null | undefined): boolean {
  if (!current?.last_probe_at || current.last_probe_status !== 'succeeded') return false;
  return Date.parse(current.last_probe_at) >= Date.parse(current.updated_at);
}

export function capabilitiesCertified(current: ProviderReadiness | null | undefined): boolean {
  if (!current) return false;
  return (
    current.enabled_model_count > 0 &&
    current.capability_count > 0 &&
    current.capability_count === current.certified_capability_count
  );
}

export function activationReady(current: ProviderReadiness | null | undefined): boolean {
  return Boolean(current?.state === 'draft' && capabilitiesCertified(current) && probeReady(current));
}

export function providerStatus(current: ProviderStatusValue): string {
  return current.pending_activation
    ? `revision ${current.active_revision} live · changes pending`
    : current.active_revision != null
      ? `revision ${current.active_revision} active`
      : current.state;
}
