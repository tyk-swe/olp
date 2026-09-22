import type { components } from '$lib/api/schema';

type Dialect = components['schemas']['SimulationDialect'];

/** The management inspector admits these native request dialects. */
export function inspectionDialects(
  operation: string,
  surface: string
): Dialect[] {
  if (operation === 'generation') {
    if (surface === 'openai') return ['openai-chat', 'openai-responses'];
    if (surface === 'anthropic') return ['anthropic-messages'];
    if (surface === 'gemini') return ['gemini-generate-content'];
  }
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
