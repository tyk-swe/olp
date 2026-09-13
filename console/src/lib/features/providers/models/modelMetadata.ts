import type { components } from '$lib/api/schema';
import { formatDate } from '$lib/format';

type Schemas = components['schemas'];

export type ModelMetadata = Schemas['ModelMetadata'];

/** One secondary upstream fact, ready to render in a per-model disclosure. */
export type MetadataFact = { label: string; value: string };

function names(values: string[]): string | null {
  return values.length ? values.join(', ') : null;
}

function flag(value: boolean | null): string | null {
  if (value === null) return null;
  return value ? 'Yes' : 'No';
}

/**
 * The upstream facts worth showing beside a model, minus the context and
 * output ceilings the table already carries as columns. Unknown fields are
 * dropped rather than rendered as blanks, so a model with nothing recorded
 * produces no disclosure at all. `supported_parameters` is the one field
 * where absence and emptiness differ: the API documents null as unknown and
 * an empty list as a model that accepts no optional parameters.
 */
export function metadataFacts(metadata: ModelMetadata): MetadataFact[] {
  const entries: readonly (readonly [string, string | null])[] = [
    ['Canonical model', metadata.canonical_model],
    ['Input modalities', names(metadata.input_modalities)],
    ['Output modalities', names(metadata.output_modalities)],
    [
      'Supported parameters',
      metadata.supported_parameters === null
        ? null
        : (names(metadata.supported_parameters) ?? 'None supported')
    ],
    ['Quantization', metadata.quantization],
    ['Region', metadata.region],
    ['Deployment', metadata.deployment],
    ['Data collection', flag(metadata.data_collection)],
    ['Zero data retention', flag(metadata.zero_data_retention)],
    ['Source', metadata.source],
    ['Observed', metadata.observed_at ? formatDate(metadata.observed_at) : null]
  ];
  return entries
    .filter((entry): entry is readonly [string, string] => entry[1] !== null)
    .map(([label, value]) => ({ label, value }));
}
