import {
  certifyProviderModel,
  getProviderCapabilityOptions,
  setProviderModel,
  type CapabilityDeclaration,
  type ProviderCapabilityOptions,
  type ProviderModel
} from './models.js';
import { certificationPrerequisiteReady } from './providerReadiness.js';
import { isRefusedProblem, probeProvider } from './providers.js';
import { dialectSurface } from './profiles.js';
import { listProviderProfiles } from './providerProfiles.js';
import { ApiProblem } from '#lib/management/http.js';
import { stringifyNativeJSON } from '#lib/nativeJson.js';
import { getProvider, type Provider } from '#lib/domain/providers/api.js';

export type BulkModelTarget = {
  id: string;
  name: string;
  capabilities: CapabilityDeclaration[];
};

export type ReviewedValidationState = {
  expectedETag: string;
  mutationPending: boolean;
};

export type ReviewedValidation =
  { status: 'checked'; certified: boolean } | { status: 'stopped' };

export type ReviewedInventory = {
  items: ProviderModel[];
  provider: Provider;
};

export type PlannedValidation = {
  model: BulkModelTarget;
  capabilities: CapabilityDeclaration[];
};

export function providerTarget(provider: Provider): string {
  return stringifyNativeJSON([
    provider.id,
    provider.configuration,
    provider.draft_credential_id
  ]);
}

export function leavesOutcomeUnknown(error: unknown): boolean {
  if (!isRefusedProblem(error)) return true;
  return (
    error instanceof ApiProblem &&
    Boolean(error.problem.type?.endsWith('/idempotency_conflict'))
  );
}

function desiredOperations(
  submittedOperation: string,
  vendor: string | undefined
): string[] {
  if (submittedOperation !== 'configured') return [submittedOperation];
  return vendor === 'voyage' ? ['embeddings'] : ['generation'];
}

function suggestedCapabilities(
  desired: string[],
  surface: string,
  options: ProviderCapabilityOptions
): CapabilityDeclaration[] {
  const candidates = options.capabilities.filter(
    (tuple) =>
      desired.includes(tuple.operation) &&
      (tuple.mode === 'unary' || tuple.mode === 'streaming')
  );
  // Prefer the native surface, but some operations (embeddings on
  // Gemini, for example) are only certifiable on another surface.
  return desired.flatMap((op) => {
    const forOp = candidates.filter((tuple) => tuple.operation === op);
    const native = forOp.filter((tuple) => tuple.surface === surface);
    return native.length ? native : forOp;
  });
}

function modelCapabilities(
  model: BulkModelTarget,
  submittedOperation: string,
  vendor: string | undefined,
  suggested: CapabilityDeclaration[]
): CapabilityDeclaration[] {
  // Responses carry source and certification fields the request
  // schema refuses.
  return submittedOperation === 'configured' &&
    vendor !== 'voyage' &&
    model.capabilities.length
    ? model.capabilities
    : suggested;
}

/** The client surface that speaks the provider's dialect natively. */
async function nativeSurface(configuration: Provider['configuration']) {
  const { kind, profile_id, profile_revision } = configuration;
  if (kind === 'anthropic') return 'anthropic';
  if (kind === 'gemini' || kind === 'vertex_ai') return 'gemini';
  if (kind !== 'plugin') return 'openai';
  // A plugin profile names its dialect.
  const profile = (await listProviderProfiles()).find(
    (candidate) =>
      candidate.id === profile_id && candidate.revision === profile_revision
  );
  return dialectSurface(profile?.dialect);
}

export async function planBulkValidation(args: {
  inventory: ReviewedInventory;
  selected: readonly string[];
  operation: string;
  signal: AbortSignal;
}): Promise<PlannedValidation[]> {
  const { inventory, selected, operation, signal } = args;
  const configuration = { ...inventory.provider.configuration };
  const vendor = configuration.options?.vendor_id ?? undefined;
  const targets = selected.flatMap((id) => {
    const model = inventory.items.find((candidate) => candidate.id === id);
    return model
      ? [
          {
            id,
            name: model.display_name,
            capabilities: model.capabilities.map(
              ({ operation, surface, mode }) => ({ operation, surface, mode })
            )
          }
        ]
      : [];
  });
  const options = await getProviderCapabilityOptions(
    configuration.kind,
    signal
  );
  const suggested = suggestedCapabilities(
    desiredOperations(operation, vendor),
    await nativeSurface(configuration),
    options
  );
  return targets.map((model) => ({
    model,
    capabilities: modelCapabilities(model, operation, vendor, suggested)
  }));
}

export async function validateReviewedModel(args: {
  providerId: string;
  model: BulkModelTarget;
  capabilities: CapabilityDeclaration[];
  reviewedMatch: (current: Provider) => boolean;
  state: ReviewedValidationState;
  signal: AbortSignal;
  canContinue: () => boolean;
  isCurrent: () => boolean;
  onCommitted: () => void;
}): Promise<ReviewedValidation> {
  const {
    providerId,
    model,
    capabilities,
    reviewedMatch,
    state,
    signal,
    canContinue,
    isCurrent,
    onCommitted
  } = args;
  let current = await getProvider(providerId, signal);
  if (!canContinue()) return { status: 'stopped' };
  if (!reviewedMatch(current)) return { status: 'stopped' };
  if (!certificationPrerequisiteReady(current)) {
    state.mutationPending = true;
    const probe = await probeProvider(current);
    state.mutationPending = false;
    state.expectedETag = probe.etag;
    if (!canContinue()) return { status: 'stopped' };
    if (!probe.result.succeeded) throw new Error(probe.result.detail);
    current = await getProvider(providerId, signal);
    if (!canContinue()) return { status: 'stopped' };
    if (!reviewedMatch(current)) return { status: 'stopped' };
  }
  state.mutationPending = true;
  current = await setProviderModel(current, model.id, true, capabilities);
  state.mutationPending = false;
  state.expectedETag = current.etag;
  if (isCurrent()) onCommitted();
  if (!canContinue()) return { status: 'stopped' };
  state.mutationPending = true;
  const certification = await certifyProviderModel(current, model.id, signal);
  state.mutationPending = false;
  state.expectedETag = certification.etag;
  if (!canContinue()) return { status: 'stopped' };
  const outcome = certification.result;
  return {
    status: 'checked',
    certified:
      Boolean(outcome.results.length) &&
      outcome.results.every((item) => item.succeeded)
  };
}
