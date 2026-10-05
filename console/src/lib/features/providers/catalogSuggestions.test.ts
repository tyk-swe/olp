import { describe, expect, it } from 'vitest';
import type { CatalogSuggestion } from '$lib/features/providers/api/catalog';
import {
  acceptableSuggestions,
  capabilityHints,
  changedFactLabels
} from './catalogSuggestions';

function suggestion(overrides: Partial<CatalogSuggestion>): CatalogSuggestion {
  return {
    model_id: 'm1',
    upstream_model: 'claude-sonnet-4-5',
    catalog_model: 'claude-sonnet-4-5-20250929',
    matched_by: 'alias',
    facts: {
      canonical_model: 'anthropic/claude-sonnet-4.5',
      context_length: 200000,
      max_output_tokens: 64000,
      input_modalities: ['image', 'text'],
      output_modalities: ['text'],
      supported_parameters: null,
      quantization: null,
      region: null,
      data_collection: null,
      zero_data_retention: null,
      deployment: null,
      source: `catalog@${'a'.repeat(64)}`,
      observed_at: '2026-10-05T12:00:00Z'
    },
    changes: ['context_length'],
    capabilities: { tools: true, reasoning: false },
    lifecycle: null,
    conflict: null,
    ...overrides
  };
}

describe('catalog suggestions', () => {
  it('offers only unconflicted suggestions that change a fact', () => {
    const items = [
      suggestion({}),
      suggestion({ upstream_model: 'same', changes: [] }),
      suggestion({ upstream_model: 'private', conflict: 'privacy_evidence' })
    ];
    expect(
      acceptableSuggestions(items).map((item) => item.upstream_model)
    ).toEqual(['claude-sonnet-4-5']);
  });

  it('names changed facts and documented capability hints', () => {
    expect(
      changedFactLabels(
        suggestion({ changes: ['context_length', 'input_modalities'] })
      )
    ).toEqual(['Context length', 'Input modalities']);
    expect(capabilityHints(suggestion({}))).toEqual(['Tools']);
  });
});
