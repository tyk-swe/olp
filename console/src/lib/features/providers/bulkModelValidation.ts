import type { CapabilityDeclaration } from './models.js';
import {
  certifyProviderModel,
  setProviderModel,
  type ProviderCapabilityOptions
} from './models.js';
import { certificationPrerequisiteReady } from './providerEditor.js';
import { getProvider, probeProvider, type Provider } from './providers.js';

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

export function desiredOperations(
  submittedOperation: string,
  vendor: string | undefined
): string[] {
  if (submittedOperation !== 'configured') return [submittedOperation];
  return vendor === 'voyage' ? ['embeddings'] : ['generation'];
}

export function suggestedCapabilities(
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

export function modelCapabilities(
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
