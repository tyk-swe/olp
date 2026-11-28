import type { ProviderKind } from '#lib/domain/providers/api.js';
import type { ProviderProbe } from './providers';
import { stateLabel } from '#lib/format.js';

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
