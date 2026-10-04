import { describe, expect, it } from 'vitest';
import type { components } from '$lib/api/schema';
import {
  decisionRows,
  describeEstimate,
  simulationRows
} from '$lib/features/routes/routingExplanation';

type Decision = components['schemas']['RoutingDecision'];

const decision: Decision = {
  target_id: 'target-a',
  provider_id: 'provider-a',
  upstream_model: 'gpt-4o',
  eligible: true,
  priority: 0,
  strategy: 'weighted',
  attempt: 1
};

describe('describeEstimate', () => {
  it('says how an exact count was made and for which family', () => {
    expect(
      describeEstimate({
        ...decision,
        estimated_input_tokens: 1204,
        estimate_provenance: 'tokenizer',
        model_family: 'openai-o200k'
      })
    ).toBe('1,204 input tokens · exact count · openai-o200k');
  });

  it('names each way an estimate can be made', () => {
    const how = (estimate_provenance: string) =>
      describeEstimate({
        ...decision,
        estimated_input_tokens: 100,
        estimate_provenance,
        model_family: 'anthropic'
      });
    expect(how('calibrated')).toBe(
      '100 input tokens · calibrated count · anthropic'
    );
    expect(how('heuristic')).toBe(
      '100 input tokens · four characters per token · anthropic'
    );
    // A method this console does not know is shown as the API named it.
    expect(how('measured')).toBe('100 input tokens · measured · anthropic');
  });

  it('claims nothing about a figure the caller supplied', () => {
    expect(
      describeEstimate({
        ...decision,
        estimated_input_tokens: 50,
        estimate_provenance: null,
        model_family: null
      })
    ).toBe('50 input tokens');
  });

  it('says nothing when nothing was estimated', () => {
    expect(describeEstimate(decision)).toBeNull();
    expect(
      describeEstimate({ ...decision, estimated_input_tokens: null })
    ).toBeNull();
  });
});

describe('explanation rows', () => {
  it('carry the estimate of each decision', () => {
    const [row] = decisionRows([
      {
        ...decision,
        estimated_input_tokens: 7,
        estimate_provenance: 'heuristic',
        model_family: 'other'
      }
    ]);
    expect(row.estimate).toBe(
      '7 input tokens · four characters per token · other'
    );
  });

  it('carry none for a target the simulator never reached', () => {
    const rows = simulationRows([
      {
        target_id: 'target-a',
        provider_id: 'provider-a',
        provider_name: 'OpenAI',
        provider_model: 'gpt-4o',
        priority: 0,
        eligible: false,
        reason: 'target_unknown',
        decision: null
      },
      {
        target_id: 'target-b',
        provider_id: 'provider-b',
        provider_name: 'Anthropic',
        provider_model: 'claude-sonnet-4-5',
        priority: 1,
        eligible: true,
        decision: {
          ...decision,
          target_id: 'target-b',
          estimated_input_tokens: 12,
          estimate_provenance: 'heuristic',
          model_family: 'anthropic'
        }
      }
    ]);
    expect(rows.map((row) => row.estimate)).toEqual([
      null,
      '12 input tokens · four characters per token · anthropic'
    ]);
  });
});
