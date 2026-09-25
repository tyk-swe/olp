import type { components } from '$lib/api/schema';

type Schemas = components['schemas'];
type RoutingDecision = Schemas['RoutingDecision'];
type SimulationTarget = Schemas['RouteSimulationResponse']['targets'][number];

/**
 * One row of an attempt-order explanation, normalized so the same view renders
 * a published-runtime explanation (which identifies providers by id) and a
 * draft simulation (which names them). A row exists for every considered
 * target, including the ones no attempt reached.
 */
export type ExplanationRow = {
  key: string;
  title: string;
  attempt: number | null;
  eligible: boolean;
  reason: string | null;
  priority: number | null;
  vendorId: string | null;
  strategy: string | null;
  providerLabel: string;
  credentialSlotId: string | null;
  price: RoutingDecision['price'];
  performance: RoutingDecision['performance'];
  metadataObservedAt: string | null;
  interaction: RoutingDecision['interaction'];
  /** Delivery evidence of an attempt that ran; absent on planned rows. */
  execution: RoutingDecision['execution'];
  /** Recorded outcome facts of an attempt that ran; absent on planned rows. */
  outcome: RoutingDecision['outcome'];
  incompatibility: RoutingDecision['incompatibility'];
};

function decisionRow(
  decision: RoutingDecision,
  title: string,
  providerLabel: string
): ExplanationRow {
  return {
    key: `${decision.target_id}:${decision.credential_slot_id ?? 'connection'}`,
    title,
    attempt: decision.attempt ?? null,
    eligible: decision.eligible,
    reason: decision.reason ?? null,
    priority: decision.priority,
    vendorId: decision.vendor_id ?? null,
    strategy: decision.strategy,
    providerLabel,
    credentialSlotId: decision.credential_slot_id ?? null,
    price: decision.price,
    performance: decision.performance,
    metadataObservedAt: decision.metadata_observed_at ?? null,
    interaction: decision.interaction,
    execution: decision.execution,
    outcome: decision.outcome,
    incompatibility: decision.incompatibility
  };
}

/**
 * Rows for an explanation that carries only decisions — the playground result
 * and the published-runtime dry run. `providerNames` resolves connection ids to
 * their display names when the caller already holds the provider list.
 */
export function decisionRows(
  decisions: RoutingDecision[],
  providerNames?: Map<string, string>
): ExplanationRow[] {
  return decisions.map((decision) => {
    const name = providerNames?.get(decision.provider_id);
    return decisionRow(
      decision,
      `${name ?? decision.provider_id} · ${decision.upstream_model}`,
      name ?? decision.provider_id
    );
  });
}

/**
 * Rows for a draft simulation. The envelope names every target the simulator
 * considered and carries its own eligibility verdict, so a target the selector
 * excluded before producing a decision still gets a row explaining why.
 */
export function simulationRows(targets: SimulationTarget[]): ExplanationRow[] {
  return targets.map((target) => {
    const title = `${target.provider_name} · ${target.provider_model}`;
    if (target.decision) {
      return {
        ...decisionRow(target.decision, title, target.provider_name),
        attempt: target.attempt ?? target.decision.attempt ?? null,
        eligible: target.eligible,
        reason: target.reason ?? target.decision.reason ?? null,
        priority: target.priority
      };
    }
    return {
      key: `${target.target_id}:connection`,
      title,
      attempt: target.attempt ?? null,
      eligible: target.eligible,
      reason: target.reason ?? null,
      priority: target.priority,
      vendorId: null,
      strategy: null,
      providerLabel: target.provider_name,
      credentialSlotId: null,
      price: null,
      performance: null,
      metadataObservedAt: null,
      interaction: undefined,
      execution: undefined,
      outcome: undefined,
      incompatibility: undefined
    };
  });
}

/** Attempted targets first in attempt order, then the excluded ones. */
export function orderedRows(rows: ExplanationRow[]): ExplanationRow[] {
  return [...rows].sort(
    (a, b) => (a.attempt ?? Infinity) - (b.attempt ?? Infinity)
  );
}
