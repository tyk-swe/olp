import { describe, expect, it } from 'vitest';

import { parseSelectors } from './routeBehavior';

describe('route selector input', () => {
  it('accepts empty input and valid model and plugin constraints', () => {
    expect(parseSelectors('  ')).toEqual([]);
    const selectors = [
      {
        id: 'large',
        when: {
          operations: ['generation'],
          streaming: true,
          min_input_tokens: 10,
          classifier: {
            route: 'classifier',
            labels: ['technical'],
            timeout_ms: 1000,
            min_score: 0.8
          }
        },
        tags: ['large']
      },
      { id: 'plugin', when: { plugin: { digest: 'digest' } }, route: 'other' }
    ];
    expect(parseSelectors(JSON.stringify(selectors))).toEqual(selectors);
  });

  it.each([
    '{',
    '{}',
    '[{"id":"one"}]',
    '[{"id":"one","when":{"streaming":"yes"}}]',
    '[{"id":"one","when":{"min_input_tokens":1.5}}]',
    '[{"id":"one","when":{"operations":[1]}}]',
    '[{"id":"one","when":{"unknown":true}}]',
    '[{"id":"one","when":{"plugin":{"digest":"x","other":1}}}]',
    '[{"id":"one","when":{"classifier":{"route":"x","labels":["ok"],"timeout_ms":1,"extra":true}}}]',
    '[{"id":"one","when":{},"tags":[1]}]',
    '[{"id":"one","when":{},"other":true}]'
  ])('refuses malformed selector input %s', (text) => {
    expect(parseSelectors(text)).toMatch(/selectors must be/i);
  });
});
