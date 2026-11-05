import type { components } from '$lib/management/schema';

export type OperationDialect = components['schemas']['OperationDialect'];
import { operationOptions, surfacesFor } from './routeEditor';

export function discoveredOperations(
  dialects: OperationDialect[]
): readonly (readonly [string, string])[] {
  const known = operationOptions.map(([id]) => id as string);
  return [
    ...operationOptions,
    ...[...new Set(dialects.map((item) => item.operation))]
      .filter((id) => !known.includes(id))
      .map((id) => [id, id.replaceAll('_', ' ')] as const)
  ];
}

export function discoveredSurfaces(
  operation: string,
  dialects: OperationDialect[]
): string[] {
  const registered = dialects.filter((item) => item.operation === operation);
  return registered.length
    ? [
        ...new Set([
          ...surfacesFor(operation),
          ...registered.map((item) => item.surface),
          'native'
        ])
      ]
    : surfacesFor(operation);
}

export function inspectionDialects(
  operation: string,
  surface: string,
  registered: OperationDialect[] = []
): string[] {
  const matches = registered.filter(
    (item) =>
      item.operation === operation &&
      (item.surface === surface || surface === 'native')
  );
  if (matches.length) return matches.map((item) => item.id);
  if (operation === 'generation') {
    if (surface === 'openai')
      return ['openai-chat', 'openai-responses', 'openai-completions'];
    if (surface === 'anthropic') return ['anthropic-messages'];
    if (surface === 'gemini') return ['gemini-generate-content'];
  }
  if (operation === 'decision' && surface === 'openai')
    return ['openai-decisions', 'systemone'];
  if (operation === 'ocr' && surface === 'openai') return ['mistral-ocr'];
  if (operation === 'search' && surface === 'openai')
    return ['perplexity-search'];
  if (operation === 'token_count') {
    if (surface === 'openai') return ['openai-input-tokens'];
    if (surface === 'anthropic') return ['anthropic-count-tokens'];
    if (surface === 'gemini') return ['gemini-count-tokens'];
  }
  if (surface === 'openai') {
    if (operation === 'embeddings') return ['openai-embeddings'];
    if (operation === 'moderation') return ['openai-moderation'];
    if (operation === 'rerank') return ['rerank'];
  }
  return [];
}
