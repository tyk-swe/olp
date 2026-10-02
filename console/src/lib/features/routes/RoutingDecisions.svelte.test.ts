import { flushSync, mount, unmount } from 'svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { components } from '$lib/api/schema';
import { decisionRows } from '$lib/features/routes/routingExplanation';
import RoutingDecisions from './RoutingDecisions.svelte';

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

let host: HTMLElement;
let component: ReturnType<typeof mount>;

beforeEach(() => {
  host = document.createElement('div');
  document.body.append(host);
});

afterEach(() => {
  void unmount(component);
  host.remove();
});

function render(decisions: Decision[]) {
  component = mount(RoutingDecisions, {
    target: host,
    props: { rows: decisionRows(decisions) }
  });
  flushSync();
}

const labels = () =>
  [...host.querySelectorAll('dt')].map((term) => term.textContent);

describe('RoutingDecisions', () => {
  it('shows how the input of each target was estimated', () => {
    render([
      {
        ...decision,
        estimated_input_tokens: 1204,
        estimate_provenance: 'tokenizer',
        model_family: 'openai-o200k'
      },
      {
        ...decision,
        target_id: 'target-b',
        upstream_model: 'claude-sonnet-4-5',
        attempt: 2,
        estimated_input_tokens: 400,
        estimate_provenance: 'heuristic',
        model_family: 'anthropic'
      }
    ]);
    expect(labels().filter((term) => term === 'Estimated input')).toHaveLength(
      2
    );
    const estimates = [...host.querySelectorAll('dt')]
      .filter((term) => term.textContent === 'Estimated input')
      .map((term) => term.nextElementSibling?.textContent);
    expect(estimates).toEqual([
      '1,204 input tokens · exact count · openai-o200k',
      '400 input tokens · four characters per token · anthropic'
    ]);
  });

  it('shows no estimate for a target that has none', () => {
    render([decision]);
    expect(labels()).not.toContain('Estimated input');
    expect(labels()).toContain('Price');
  });
});
