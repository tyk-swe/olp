import { mount, unmount } from 'svelte';
import { expect, it, onTestFinished } from 'vitest';
import type { components } from '#lib/management/schema.js';
import RoutingDecisions from './RoutingDecisions.svelte';
import { decisionRows } from './routingExplanation.js';

const decision: components['schemas']['RoutingDecision'] = {
  target_id: 'target',
  provider_id: 'provider',
  upstream_model: 'fixture-model',
  eligible: true,
  attempt: 1,
  priority: 1,
  strategy: 'weighted',
  estimated_input_tokens: 1200,
  estimate_provenance: 'calibrated',
  context_length: 1400,
  requested_output_tokens: 256,
  context_window: {
    input_breakdown: {
      instructions: 10,
      conversation: 50,
      tool_schemas: 1100,
      tool_calls_results: 30,
      media: 0,
      unclassified: 10
    },
    remaining_tokens: -56,
    retained_context_unknown: false
  }
};

it('shows the dominant input category, estimate source and how far the request is over the window', () => {
  const panel = preview(decision);
  expect(panel.textContent).toContain('No billed usage.');
  expect(panel.textContent).toContain('1,200 input tokens · calibrated count');
  expect(panel.textContent).toContain('1,400 tokens');
  expect(panel.textContent).toContain('256 tokens');
  expect(panel.textContent).toContain('56 tokens over the limit');
  const categories = [...panel.querySelectorAll('h5 + dl dt')];
  expect(categories[0]?.textContent).toBe('Tool schemas');
  expect(categories[0]?.nextElementSibling?.textContent).toBe('1,100');
});

it('does not display an unknown limit or unmeasured context as zero', () => {
  const panel = preview({
    ...decision,
    context_length: null,
    context_window: {
      input_breakdown: null,
      remaining_tokens: null,
      retained_context_unknown: true
    },
    requested_output_tokens: null,
    estimate_provenance: null
  });
  const value = (label: string) =>
    [...panel.querySelectorAll('dt')]
      .find((term) => term.textContent === label)
      ?.nextElementSibling?.textContent?.trim();
  expect(value('Model limit')).toBe('Unknown');
  expect(value('Headroom after input')).toBe('Unknown');
  expect(value('Output reservation')).toBe('No bound applied');
  expect(value('Provider-held context')).toContain('Unknown');
  expect(value('Estimated input')).toContain('caller estimate');
});

function preview(row: typeof decision): HTMLElement {
  const target = document.createElement('div');
  const component = mount(RoutingDecisions, {
    target,
    props: { rows: decisionRows([row]) }
  });
  onTestFinished(() => unmount(component));
  const panel = target.querySelector<HTMLElement>(
    'section[aria-label="Context window"]'
  );
  if (!panel) throw new Error('Missing context window preview');
  return panel;
}
