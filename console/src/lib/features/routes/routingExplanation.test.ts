import { describe, expect, it } from 'vitest';
import type { components } from '$lib/management/schema';
import {
  adaptiveNotes,
  decisionRows,
  describeEstimate,
  orderedRows,
  simulationRows
} from './routingExplanation';

type Schemas = components['schemas'];

const decision: Schemas['RoutingDecision'] = {
  target_id: 'target-a',
  provider_id: 'provider-a',
  upstream_model: 'model-a',
  eligible: true,
  priority: 2,
  strategy: 'latency',
  attempt: 2,
  credential_slot_id: 'slot-a',
  estimated_input_tokens: 1200,
  estimate_provenance: 'tokenizer',
  model_family: 'family-a',
  estimated_cost: { minimum: '0.001', maximum: '0.01', currency: 'USD' }
};

describe('routing explanations', () => {
  it('identifies adaptive routing choices and shows zero headroom', () => {
    expect(adaptiveNotes(decision)).toEqual([]);
    expect(
      adaptiveNotes({
        ...decision,
        shadow: true,
        selector: 'region',
        unhealthy: true,
        headroom: 0
      })
    ).toEqual([
      'Shadow target: mirrors, never serves',
      'Chosen by selector region',
      'Unhealthy across the fleet, so ordered last',
      '0% capacity headroom'
    ]);
    expect(adaptiveNotes({ ...decision, headroom: 0.456 })).toEqual([
      '46% capacity headroom'
    ]);
  });

  it.each([
    ['tokenizer', 'exact count'],
    ['calibrated', 'calibrated count'],
    ['heuristic', 'four characters per token'],
    ['other', 'other']
  ])(
    'describes the %s estimate with its provenance',
    (estimate_provenance, label) => {
      expect(describeEstimate({ ...decision, estimate_provenance })).toBe(
        `1,200 input tokens · ${label} · family-a`
      );
    }
  );

  it('does not invent an estimate or its provenance', () => {
    expect(
      describeEstimate({ ...decision, estimated_input_tokens: null })
    ).toBeNull();
    expect(
      describeEstimate({
        ...decision,
        estimated_input_tokens: 0,
        estimate_provenance: null,
        model_family: null
      })
    ).toBe('0 input tokens');
  });

  it('uses known provider names and retains each decision detail', () => {
    const [row] = decisionRows(
      [decision],
      new Map([['provider-a', 'Provider A']])
    );
    expect(row).toMatchObject({
      key: 'target-a:slot-a',
      title: 'Provider A · model-a',
      providerLabel: 'Provider A',
      attempt: 2,
      eligible: true,
      priority: 2,
      strategy: 'latency',
      credentialSlotId: 'slot-a',
      estimate: '1,200 input tokens · exact count · family-a',
      estimatedCost: decision.estimated_cost
    });
    expect(
      decisionRows([{ ...decision, credential_slot_id: null }])[0]
    ).toMatchObject({
      key: 'target-a:connection',
      title: 'provider-a · model-a',
      providerLabel: 'provider-a'
    });
  });

  it('includes excluded targets and lets simulation verdicts override decisions', () => {
    const included: Schemas['RouteSimulationTargetResponse'] = {
      target_id: 'target-a',
      provider_id: 'provider-a',
      provider_name: 'Provider A',
      provider_model: 'model-a',
      eligible: false,
      reason: 'policy denied',
      priority: 5,
      attempt: 1,
      decision
    };
    const excluded: Schemas['RouteSimulationTargetResponse'] = {
      target_id: 'target-b',
      provider_id: 'provider-b',
      provider_name: 'Provider B',
      provider_model: 'model-b',
      eligible: false,
      reason: 'not selected',
      priority: 3
    };
    expect(simulationRows([included, excluded])).toMatchObject([
      {
        key: 'target-a:slot-a',
        title: 'Provider A · model-a',
        attempt: 1,
        eligible: false,
        reason: 'policy denied',
        priority: 5,
        estimate: '1,200 input tokens · exact count · family-a'
      },
      {
        key: 'target-b:connection',
        title: 'Provider B · model-b',
        attempt: null,
        eligible: false,
        reason: 'not selected',
        priority: 3,
        estimate: null,
        notes: []
      }
    ]);
  });

  it('orders attempted targets before excluded targets without changing the input', () => {
    const rows = decisionRows([
      { ...decision, target_id: 'unattempted', attempt: null },
      { ...decision, target_id: 'second', attempt: 2 },
      { ...decision, target_id: 'first', attempt: 1 }
    ]);
    expect(orderedRows(rows).map((row) => row.key)).toEqual([
      'first:slot-a',
      'second:slot-a',
      'unattempted:slot-a'
    ]);
    expect(rows.map((row) => row.key)).toEqual([
      'unattempted:slot-a',
      'second:slot-a',
      'first:slot-a'
    ]);
  });
});
