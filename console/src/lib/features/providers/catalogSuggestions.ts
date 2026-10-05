import type { CatalogSuggestion } from '$lib/features/providers/api/catalog';

const FACT_LABELS: Record<string, string> = {
  canonical_model: 'Canonical model',
  context_length: 'Context length',
  max_output_tokens: 'Max output tokens',
  input_modalities: 'Input modalities',
  output_modalities: 'Output modalities',
  supported_parameters: 'Supported parameters'
};

/** The suggestions an operator can accept: unconflicted ones that change something. */
export function acceptableSuggestions(
  items: readonly CatalogSuggestion[]
): CatalogSuggestion[] {
  return items.filter((item) => !item.conflict && item.changes.length > 0);
}

/** The facts a suggestion would change, as the console names them. */
export function changedFactLabels(item: CatalogSuggestion): string[] {
  return item.changes.map((name) => FACT_LABELS[name] ?? name);
}

/** The capability hints the vendor documents for a model. */
export function capabilityHints(item: CatalogSuggestion): string[] {
  const hints: [boolean | undefined, string][] = [
    [item.capabilities.tools, 'Tools'],
    [item.capabilities.structured_outputs, 'Structured outputs'],
    [item.capabilities.reasoning, 'Reasoning'],
    [item.capabilities.prompt_caching, 'Prompt caching']
  ];
  return hints.filter(([value]) => value === true).map(([, label]) => label);
}
