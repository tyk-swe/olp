import type {
  CreateProviderInput,
  ProviderAuthMode,
  ProviderKind,
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
  invalid_enabled_model_count: number;
  probe_ready: boolean;
  last_probe_at?: string | null;
  last_probe_status?: string | null;
};

export type ProviderStatusValue = Pick<ProviderReadiness, 'state'> & {
  active_revision?: number | null;
  pending_activation: boolean;
};

type AuthOption = readonly [ProviderAuthMode, string];

const apiKeyAuthOptions: readonly AuthOption[] = [['api_key', 'Stored API key']];

export function createProviderDraft(): ProviderDraft {
  return {
    kind: 'open_ai',
    name: '',
    endpoint: '',
    apiVersion: '',
    cloudRegion: '',
    cloudProject: '',
    deployment: '',
    authMode: 'api_key',
    credential: '',
    model: ''
  };
}

export function authOptionsFor(kind: ProviderKind): readonly AuthOption[] {
  if (kind === 'vertex_ai') {
    return [
      ['adc', 'Application Default Credentials'],
      ['service_account', 'Stored service account JSON']
    ] as const;
  }
  if (kind === 'bedrock') {
    return [
      ['default_chain', 'AWS default chain'],
      ['static', 'Stored static AWS credential']
    ] as const;
  }
  return apiKeyAuthOptions;
}

export function requiresCredential(authMode: ProviderAuthMode): boolean {
  return !['adc', 'default_chain'].includes(authMode);
}

export function requiresSeedModel(kind: ProviderKind): boolean {
  return kind === 'vertex_ai';
}

export function hasCustomEndpoint(kind: ProviderKind): boolean {
  return ['open_ai_compatible', 'azure_open_ai'].includes(kind);
}

export function hasCloudRegion(kind: ProviderKind): boolean {
  return ['vertex_ai', 'bedrock'].includes(kind);
}

export function hasCloudProject(kind: ProviderKind): boolean {
  return kind === 'vertex_ai';
}

export function hasDeployment(kind: ProviderKind): boolean {
  return kind === 'azure_open_ai';
}

export function hasApiVersion(kind: ProviderKind): boolean {
  return kind === 'azure_open_ai';
}

export function supportsManualModelDeclaration(kind: ProviderKind): boolean {
  return !['open_ai', 'anthropic', 'gemini'].includes(kind);
}

export function validateProviderDraft(draft: ProviderDraft): string | null {
  if (
    !draft.name.trim() ||
    (requiresSeedModel(draft.kind) && !draft.model.trim()) ||
    (requiresCredential(draft.authMode) && !draft.credential)
  ) {
    return requiresSeedModel(draft.kind)
      ? 'Name, Vertex probe model, and the selected identity fields are required.'
      : 'Name and the selected identity fields are required.';
  }
  if (draft.kind === 'open_ai_compatible' && !draft.endpoint.trim()) {
    return 'An HTTPS endpoint is required for an OpenAI-compatible provider.';
  }
  if (draft.kind === 'vertex_ai' && (!draft.cloudProject.trim() || !draft.cloudRegion.trim())) {
    return 'Vertex AI requires a cloud project and location.';
  }
  if (draft.kind === 'bedrock' && !draft.cloudRegion.trim()) {
    return 'AWS Bedrock requires a cloud region.';
  }
  if (
    draft.kind === 'azure_open_ai' &&
    (!draft.endpoint.trim() || !draft.deployment.trim() || !draft.apiVersion.trim())
  ) {
    return 'Azure OpenAI requires its resource endpoint, deployment, and API version.';
  }
  return null;
}

export function buildCreateProviderInput(draft: ProviderDraft): CreateProviderInput {
  return {
    name: draft.name.trim(),
    kind: draft.kind,
    credential: draft.credential || undefined,
    model: draft.model.trim() || null,
    endpoint: hasCustomEndpoint(draft.kind) ? draft.endpoint.trim() || null : null,
    api_version: hasApiVersion(draft.kind) ? draft.apiVersion.trim() || null : null,
    cloud_region: hasCloudRegion(draft.kind) ? draft.cloudRegion.trim() || null : null,
    cloud_project: hasCloudProject(draft.kind) ? draft.cloudProject.trim() || null : null,
    deployment: hasDeployment(draft.kind) ? draft.deployment.trim() || null : null,
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
}): ProviderEditValues {
  return {
    name: current.name,
    endpoint: hasCustomEndpoint(current.kind) ? current.endpoint ?? '' : '',
    apiVersion: hasApiVersion(current.kind) ? current.api_version ?? '' : '',
    cloudRegion: hasCloudRegion(current.kind) ? current.cloud_region ?? '' : '',
    cloudProject: hasCloudProject(current.kind) ? current.cloud_project ?? '' : '',
    deployment: hasDeployment(current.kind) ? current.deployment ?? '' : '',
    authMode: current.auth_mode
  };
}

export function buildUpdateProviderInput(
  current: { kind: ProviderKind },
  values: ProviderEditValues
): UpdateProviderInput {
  return {
    name: values.name.trim(),
    endpoint: hasCustomEndpoint(current.kind) ? values.endpoint.trim() || null : null,
    api_version: hasApiVersion(current.kind) ? values.apiVersion.trim() || null : null,
    cloud_region: hasCloudRegion(current.kind) ? values.cloudRegion.trim() || null : null,
    cloud_project: hasCloudProject(current.kind) ? values.cloudProject.trim() || null : null,
    deployment: hasDeployment(current.kind) ? values.deployment.trim() || null : null,
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
  return Boolean(current?.probe_ready);
}

export function capabilitiesCertified(current: ProviderReadiness | null | undefined): boolean {
  if (!current) return false;
  return (
    current.enabled_model_count > 0 &&
    current.invalid_enabled_model_count === 0
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
