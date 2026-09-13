import { expect, it } from 'vitest';
import { formatDate } from '$lib/format';
import { metadataFacts, type ModelMetadata } from './modelMetadata';

const unknown: ModelMetadata = {
  canonical_model: null,
  context_length: null,
  data_collection: null,
  deployment: null,
  input_modalities: [],
  max_output_tokens: null,
  observed_at: null,
  output_modalities: [],
  quantization: null,
  region: null,
  source: null,
  supported_parameters: null,
  zero_data_retention: null
};

it('drops fields the upstream never reported', () => {
  expect(metadataFacts(unknown)).toEqual([]);
  expect(
    metadataFacts({ ...unknown, context_length: 200_000, max_output_tokens: 8 })
  ).toEqual([]);
});

it('separates an unknown parameter set from a declared empty one', () => {
  expect(metadataFacts({ ...unknown, supported_parameters: [] })).toEqual([
    { label: 'Supported parameters', value: 'None supported' }
  ]);
  expect(
    metadataFacts({
      ...unknown,
      supported_parameters: ['temperature', 'top_p']
    })
  ).toEqual([{ label: 'Supported parameters', value: 'temperature, top_p' }]);
});

it('names modalities, privacy declarations, and the observation', () => {
  expect(
    metadataFacts({
      ...unknown,
      canonical_model: 'openai/gpt-5.4',
      input_modalities: ['text', 'image'],
      output_modalities: ['text'],
      quantization: 'fp8',
      region: 'us-east-1',
      deployment: 'prod-gpt',
      data_collection: false,
      zero_data_retention: true,
      source: 'vendor documentation',
      observed_at: '2026-03-04T12:30:00Z'
    })
  ).toEqual([
    { label: 'Canonical model', value: 'openai/gpt-5.4' },
    { label: 'Input modalities', value: 'text, image' },
    { label: 'Output modalities', value: 'text' },
    { label: 'Quantization', value: 'fp8' },
    { label: 'Region', value: 'us-east-1' },
    { label: 'Deployment', value: 'prod-gpt' },
    { label: 'Data collection', value: 'No' },
    { label: 'Zero data retention', value: 'Yes' },
    { label: 'Source', value: 'vendor documentation' },
    { label: 'Observed', value: formatDate('2026-03-04T12:30:00Z') }
  ]);
});
